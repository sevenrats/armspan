#!/usr/bin/env bash
# sync-release-tags.sh — for each upstream semver tag that lacks a
# corresponding *-armspan tag, rebase the armspan delta onto it and push.
#
# Expects:
#   - upstream remote already fetched with tags
#   - main-armspan already rebased on upstream/main (current branch)
#
# Outputs:
#   GITHUB_OUTPUT: summary=<markdown table of results>

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────
# Minimum upstream tag to process. Tags older than this are skipped.
# Override via env: ARMSPAN_MIN_TAG=v0.25.0
ARMSPAN_MIN_TAG="${ARMSPAN_MIN_TAG:-v0.24.0}"

# Upstream repo URL (override for forks of forks, etc.)
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/juanfont/headscale.git}"

# GITHUB_OUTPUT may not exist when running locally.
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

# ── Helpers ──────────────────────────────────────────────────────────────
semver_re='^v[0-9]+\.[0-9]+\.[0-9]+$'

log()  { echo "::group::$*"; }
endg() { echo "::endgroup::"; }

# version_ge returns 0 (true) if $1 >= $2 using semver sort.
version_ge() {
  local highest
  highest=$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)
  [[ "$highest" == "$1" ]]
}

# ── Ensure upstream remote exists and is fetched ─────────────────────────
if ! git remote get-url upstream &>/dev/null; then
  echo "Adding upstream remote: $UPSTREAM_URL"
  git remote add upstream "$UPSTREAM_URL"
fi

echo "Fetching upstream main + tags..."
git fetch upstream main --tags --force

# ── Gather tags ──────────────────────────────────────────────────────────
mapfile -t upstream_tags < <(
  git tag -l 'v*' --sort=-v:refname |
    grep -E "$semver_re" || true
)

echo "Minimum tag floor: $ARMSPAN_MIN_TAG"

if [[ ${#upstream_tags[@]} -eq 0 ]]; then
  echo "No upstream semver tags found."
  echo "summary=" >> "$GITHUB_OUTPUT"
  exit 0
fi

echo "Found ${#upstream_tags[@]} upstream semver tags."

# Already-existing armspan tags (set for O(1) lookup)
declare -A existing_armspan
for t in $(git tag -l '*-armspan'); do
  existing_armspan["$t"]=1
done

# ── Determine the armspan delta (commits unique to main-armspan) ────────
# These are the commits between upstream/main and main-armspan.
armspan_base=$(git merge-base upstream/main HEAD)
armspan_patch_count=$(git rev-list --count "$armspan_base"..HEAD)
echo "Armspan delta: $armspan_patch_count commit(s) on top of upstream/main."

if [[ $armspan_patch_count -eq 0 ]]; then
  echo "No armspan-specific commits to apply."
  echo "summary=" >> "$GITHUB_OUTPUT"
  exit 0
fi

# ── Process each tag ────────────────────────────────────────────────────
created=()
skipped=()
failed=()

for tag in "${upstream_tags[@]}"; do
  armspan_tag="${tag}-armspan"

  # Skip tags older than the minimum floor.
  if ! version_ge "$tag" "$ARMSPAN_MIN_TAG"; then
    skipped+=("$tag (below minimum $ARMSPAN_MIN_TAG)")
    continue
  fi

  if [[ -n "${existing_armspan[$armspan_tag]:-}" ]]; then
    skipped+=("$tag (already exists)")
    continue
  fi

  log "Processing $tag → $armspan_tag"

  # Create a detached temporary branch at the upstream tag
  if ! git checkout --detach "$tag" 2>/dev/null; then
    failed+=("$tag (checkout failed)")
    endg
    continue
  fi

  # Cherry-pick the armspan delta onto this tag
  if git cherry-pick "$armspan_base"..main-armspan --no-rerere-autoupdate; then
    git tag "$armspan_tag"
    git push origin "$armspan_tag"
    created+=("$tag")
    echo "✅ Created $armspan_tag"
  else
    git cherry-pick --abort 2>/dev/null || true
    failed+=("$tag (cherry-pick conflict)")
    echo "❌ Failed $armspan_tag"
  fi

  endg
done

# Return to main-armspan
git checkout main-armspan

# ── Build summary ───────────────────────────────────────────────────────
summary=""

if [[ ${#created[@]} -gt 0 ]]; then
  summary+="### ✅ Created\n"
  for t in "${created[@]}"; do summary+="- \`${t}-armspan\`\n"; done
  summary+="\n"
fi

if [[ ${#failed[@]} -gt 0 ]]; then
  summary+="### ❌ Failed\n"
  for t in "${failed[@]}"; do summary+="- $t\n"; done
  summary+="\n"
fi

if [[ ${#skipped[@]} -gt 0 ]]; then
  summary+="### ⏭️ Skipped\n"
  for t in "${skipped[@]}"; do summary+="- $t\n"; done
fi

if [[ -n "$summary" ]]; then
  # Multi-line output via delimiter
  {
    echo "summary<<TAGSYNC_EOF"
    echo -e "$summary"
    echo "TAGSYNC_EOF"
  } >> "$GITHUB_OUTPUT"
else
  echo "summary=" >> "$GITHUB_OUTPUT"
fi

echo ""
echo "=== Tag sync complete ==="
echo "  Created: ${#created[@]}"
echo "  Failed:  ${#failed[@]}"
echo "  Skipped: ${#skipped[@]}"
