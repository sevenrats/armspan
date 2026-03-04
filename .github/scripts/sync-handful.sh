#!/usr/bin/env bash
# sync-handful.sh — rebase the handful fork onto the tailscale version
# referenced in armspan's go.mod, then tag+push with armspan's version.
#
# handful (github.com/sevenrats/handful) is a modified fork of tailscale.
# Its `main-handful` branch carries a small patch set on top of upstream
# tailscale's `main`.  For each armspan release we:
#
#   1. Read armspan's go.mod to find the tailscale.com version (e.g. v1.94.1)
#   2. In the handful repo, rebase the handful-specific commits onto
#      that upstream tailscale tag
#   3. Tag with armspan's version tag (e.g. v0.28.0)
#   4. Push the tag to sevenrats/handful
#
# Environment variables (all required):
#   ARMSPAN_TAG     — the armspan release tag (e.g. v0.28.0)
#   ARMSPAN_GOMOD   — path to armspan's go.mod file
#   HANDFUL_DIR     — path to the cloned handful repo
#   HANDFUL_PAT     — GitHub PAT with push access to sevenrats/handful

set -euo pipefail

# ── Validate inputs ──────────────────────────────────────────────────────
: "${ARMSPAN_TAG:?ARMSPAN_TAG is required}"
: "${ARMSPAN_GOMOD:?ARMSPAN_GOMOD is required}"
: "${HANDFUL_DIR:?HANDFUL_DIR is required}"
: "${HANDFUL_PAT:?HANDFUL_PAT is required}"

echo "=== sync-handful ==="
echo "  Armspan tag:  $ARMSPAN_TAG"
echo "  go.mod path:  $ARMSPAN_GOMOD"
echo "  Handful dir:  $HANDFUL_DIR"

# ── 1. Extract tailscale version from armspan's go.mod ───────────────────
# Matches a line like: tailscale.com v1.94.1
TS_VERSION=$(grep -E '^\s+tailscale\.com\s+v' "$ARMSPAN_GOMOD" | awk '{print $2}' | head -1)

if [[ -z "$TS_VERSION" ]]; then
  echo "ERROR: could not find tailscale.com dependency in $ARMSPAN_GOMOD"
  exit 1
fi

echo "  Tailscale version from go.mod: $TS_VERSION"

# ── 2. Set up handful repo ──────────────────────────────────────────────
cd "$HANDFUL_DIR"

git config user.name  "github-actions[bot]"
git config user.email "github-actions[bot]@users.noreply.github.com"

# Add upstream tailscale remote for fetching the release tag.
if ! git remote get-url upstream &>/dev/null; then
  git remote add upstream https://github.com/tailscale/tailscale.git
fi

# Configure push URL with PAT for sevenrats/handful.
git remote set-url origin "https://x-access-token:${HANDFUL_PAT}@github.com/sevenrats/handful.git"

echo "Fetching upstream tailscale tags..."
git fetch upstream --tags --force

echo "Fetching origin (sevenrats/handful)..."
git fetch origin --tags --force

# ── 3. Verify the upstream tag exists ────────────────────────────────────
if ! git rev-parse "refs/tags/${TS_VERSION}" &>/dev/null; then
  echo "ERROR: upstream tailscale tag ${TS_VERSION} not found"
  echo "  Available tags near that version:"
  git tag -l 'v1.9*' --sort=-v:refname | head -10
  exit 1
fi

echo "  Upstream tag ${TS_VERSION} found: $(git rev-parse --short "refs/tags/${TS_VERSION}")"

# ── 4. Determine the handful-specific commits ───────────────────────────
# main-handful sits on top of origin/main (which tracks upstream tailscale).
# We need the commits that are on main-handful but NOT on origin/main.

if ! git rev-parse origin/main-handful &>/dev/null; then
  echo "ERROR: origin/main-handful branch not found in handful repo"
  exit 1
fi

# Find the merge base between main-handful and origin/main (upstream).
HANDFUL_BASE=$(git merge-base origin/main origin/main-handful)
HANDFUL_COMMIT_COUNT=$(git rev-list --count "${HANDFUL_BASE}..origin/main-handful")

echo "  Handful delta: ${HANDFUL_COMMIT_COUNT} commit(s) on top of upstream"

if [[ "$HANDFUL_COMMIT_COUNT" -eq 0 ]]; then
  echo "WARNING: no handful-specific commits found.  Nothing to rebase."
  echo "  Tagging upstream ${TS_VERSION} as ${ARMSPAN_TAG} directly."

  git tag -f "$ARMSPAN_TAG" "refs/tags/${TS_VERSION}"
  git push --force origin "refs/tags/${ARMSPAN_TAG}"
  echo "✅ Pushed ${ARMSPAN_TAG} to sevenrats/handful (no handful patches)"
  exit 0
fi

# ── 5. Rebase handful commits onto the tailscale release tag ─────────────
# Strategy:
#   - Create a temporary branch from the upstream tailscale tag
#   - Rebase the handful-specific commits onto it
#   - Tag the result with armspan's version

WORK_BRANCH="tmp-handful-${ARMSPAN_TAG}"
git branch -D "$WORK_BRANCH" 2>/dev/null || true

# Start from the upstream tailscale release tag.
git checkout -b "$WORK_BRANCH" "refs/tags/${TS_VERSION}"

echo "Rebasing ${HANDFUL_COMMIT_COUNT} handful commit(s) onto ${TS_VERSION}..."

# Rebase: take commits from HANDFUL_BASE..origin/main-handful and replay
# them onto the current branch (which is at the tailscale release tag).
if ! git rebase --onto "$WORK_BRANCH" "$HANDFUL_BASE" origin/main-handful; then
  echo ""
  echo "ERROR: rebase failed — handful patches conflict with tailscale ${TS_VERSION}"
  echo ""
  echo "To resolve manually:"
  echo "  1. Clone sevenrats/handful"
  echo "  2. git fetch upstream (tailscale/tailscale)"
  echo "  3. git checkout -b fix origin/main-handful"
  echo "  4. git rebase --onto ${TS_VERSION} \$(git merge-base origin/main origin/main-handful)"
  echo "  5. Resolve conflicts, then tag as ${ARMSPAN_TAG} and push"
  git rebase --abort 2>/dev/null || true
  exit 1
fi

echo "  Rebase succeeded."

# ── 6. Tag and push ─────────────────────────────────────────────────────
git tag -f "$ARMSPAN_TAG"

echo "Pushing tag ${ARMSPAN_TAG} to sevenrats/handful..."
if git push --force origin "refs/tags/${ARMSPAN_TAG}"; then
  echo "✅ Pushed ${ARMSPAN_TAG} to sevenrats/handful"
  echo "   Based on tailscale ${TS_VERSION} + ${HANDFUL_COMMIT_COUNT} handful patch(es)"
else
  echo "❌ Failed to push ${ARMSPAN_TAG} to sevenrats/handful"
  exit 1
fi

# ── Cleanup ──────────────────────────────────────────────────────────────
git checkout --detach 2>/dev/null || true
git branch -D "$WORK_BRANCH" 2>/dev/null || true

echo ""
echo "=== sync-handful complete ==="
echo "  Armspan tag:    ${ARMSPAN_TAG}"
echo "  Tailscale base: ${TS_VERSION}"
echo "  Handful patches: ${HANDFUL_COMMIT_COUNT}"
