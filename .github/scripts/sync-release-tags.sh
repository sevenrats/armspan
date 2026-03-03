#!/usr/bin/env bash
# sync-release-tags.sh — for each upstream semver tag >= a minimum floor,
# build an armspan release and push the plain semver tag to the github remote.
#
# Strategy (conflict-free):
#   1. Checkout the upstream release tag (from the upstream remote)
#   2. Copy all NEW files from main-armspan (git diff --diff-filter=A)
#   3. sed-inject the one-line hook into hscontrol/db/db.go
#   4. Commit, force-tag as the same semver (e.g. v0.25.0), push to github
#
# The armspan github repo only ever contains plain semver tags, and these
# are always our builds — never raw upstream.  Upstream tags live only on
# the upstream remote.
#
# No cherry-pick or rebase — upstream files are never modified except
# db.go via sed, so there are zero merge conflicts.
#
# Outputs:
#   GITHUB_OUTPUT: summary=<markdown table of results>

set -uo pipefail
# NOTE: no -e — we handle errors explicitly so we can always clean up.

# ── Configuration ────────────────────────────────────────────────────────
ARMSPAN_MIN_TAG="${ARMSPAN_MIN_TAG:-v0.27.0}"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/juanfont/headscale.git}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"
GITHUB_REMOTE="${GITHUB_REMOTE:-github}"

DB_GO="hscontrol/db/db.go"

# Upstream workflows to remove from release tags (they fail on the fork).
REMOVE_WORKFLOWS=(
  .github/workflows/docs-deploy.yml
  .github/workflows/docs-test.yml
)

# Save the branch we started on so we always return to it.
start_branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "main-armspan")

cleanup() {
  git checkout "$start_branch" 2>/dev/null || true
  # Remove any leftover tmp branches
  git for-each-ref --format='%(refname:short)' refs/heads/tmp-armspan-\* |
    xargs -r git branch -D 2>/dev/null || true
}
trap cleanup EXIT

# ── Helpers ──────────────────────────────────────────────────────────────
semver_re='^v[0-9]+\.[0-9]+\.[0-9]+$'

log()  { echo "::group::$*"; }
endg() { echo "::endgroup::"; }

version_ge() {
  local highest
  highest=$(printf '%s\n%s\n' "$1" "$2" | sort -V | tail -n1)
  [[ "$highest" == "$1" ]]
}

# inject_hook uses sed to insert the hook line into db.go.
# Returns 0 if the injection was made, 1 if the anchor was not found.
inject_hook() {
  local target="$1"
  if ! grep -q 'postOpenDBHook' "$target"; then
    if grep -qP '^\s+dbConn, err := openDB' "$target"; then
      sed -i '/dbConn, err := openDB/{
        n; # if err != nil {
        n; # return nil, err
        n; # }
        a\
\tpostOpenDBHook(dbConn) // armspan: plugin hook (see gormspan_init.go)
      }' "$target"
      grep -q 'postOpenDBHook' "$target"
    else
      return 1
    fi
  fi
}

# ── Ensure remotes exist ─────────────────────────────────────────────────
if ! git remote get-url upstream &>/dev/null; then
  echo "Adding upstream remote: $UPSTREAM_URL"
  git remote add upstream "$UPSTREAM_URL"
fi

if ! git remote get-url "$GITHUB_REMOTE" &>/dev/null; then
  echo "ERROR: remote '$GITHUB_REMOTE' does not exist. Set GITHUB_REMOTE or add it."
  exit 1
fi
echo "Push target: $(git remote get-url "$GITHUB_REMOTE")"

echo "Fetching upstream tags..."
git fetch upstream --tags --force || { echo "ERROR: failed to fetch upstream"; exit 1; }

# ── Gather upstream semver tags ──────────────────────────────────────────
# List tags reachable from the upstream remote only.
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

# ── Tags already on github remote ───────────────────────────────────────
# We skip tags that already exist on the github remote (already pushed).
declare -A github_tags=()
while IFS=$'\t' read -r _sha ref; do
  [[ -z "$ref" ]] && continue
  t="${ref#refs/tags/}"
  github_tags["$t"]=1
done < <(git ls-remote --tags "$GITHUB_REMOTE" 2>/dev/null | grep -v '\^{}' || true)

echo "Tags already on github remote: ${#github_tags[@]}"

# ── Determine the armspan delta ─────────────────────────────────────────
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
  if ! version_ge "$tag" "$ARMSPAN_MIN_TAG"; then
    skipped+=("$tag (below minimum $ARMSPAN_MIN_TAG)")
    continue
  fi

  if [[ -n "${github_tags[$tag]:-}" ]]; then
    skipped+=("$tag (already on github)")
    continue
  fi

  log "Processing $tag"

  # 1. Start from the upstream release tag.
  tmp_branch="tmp-armspan-${tag}"
  git branch -D "$tmp_branch" 2>/dev/null || true

  if ! git checkout "refs/tags/${tag}" --detach; then
    failed+=("$tag (checkout failed)")
    echo "❌ Failed $tag — could not checkout tag"
    endg; continue
  fi

  if ! git checkout -b "$tmp_branch"; then
    failed+=("$tag (branch create failed)")
    echo "❌ Failed $tag — could not create tmp branch"
    endg; continue
  fi

  # 2. Copy armspan-only NEW files from main-armspan onto this tag.
  mapfile -t new_files < <(
    git diff --name-only --diff-filter=A "$armspan_base" main-armspan
  )

  if [[ ${#new_files[@]} -gt 0 ]]; then
    if ! git checkout main-armspan -- "${new_files[@]}"; then
      failed+=("$tag (file copy failed)")
      echo "❌ Failed $tag — could not copy armspan files"
      endg; continue
    fi
    git add "${new_files[@]}"
    git commit -m "armspan: add gormspan plugin and CI infrastructure" --no-verify
  fi

  # 2b. Remove upstream workflows that should not run on the fork.
  removed=()
  for wf in "${REMOVE_WORKFLOWS[@]}"; do
    if [[ -f "$wf" ]]; then
      git rm -f "$wf"
      removed+=("$wf")
    fi
  done
  if [[ ${#removed[@]} -gt 0 ]]; then
    git commit -m "armspan: remove upstream docs workflows" --no-verify
  fi

  # 3. Inject the hook into db.go via sed.
  if ! inject_hook "$DB_GO"; then
    failed+=("$tag (db.go anchor not found — upstream changed)")
    echo "❌ Failed $tag (sed: openDB anchor missing)"
    endg; continue
  fi

  # 4. Commit the db.go hook, force-tag as plain semver, push to github.
  git add "$DB_GO"
  git commit -m "armspan: hook gormspan into db.go" --no-verify

  git tag -f "$tag"

  if git push --force "$GITHUB_REMOTE" "refs/tags/${tag}"; then
    created+=("$tag")
    echo "✅ Pushed $tag to $GITHUB_REMOTE"
  else
    failed+=("$tag (push failed)")
    echo "❌ Failed $tag — push to $GITHUB_REMOTE failed"
  fi

  # Clean up tmp branch (cleanup trap handles this too, but be tidy).
  git checkout "$start_branch" 2>/dev/null || true
  git branch -D "$tmp_branch" 2>/dev/null || true

  endg
done

# ── Build summary ───────────────────────────────────────────────────────
summary=""

if [[ ${#created[@]} -gt 0 ]]; then
  summary+="### Created\n"
  for t in "${created[@]}"; do summary+="- \`$t\`\n"; done
  summary+="\n"
fi

if [[ ${#failed[@]} -gt 0 ]]; then
  summary+="### Failed\n"
  for t in "${failed[@]}"; do summary+="- $t\n"; done
  summary+="\n"
fi

if [[ ${#skipped[@]} -gt 0 ]]; then
  summary+="### Skipped\n"
  for t in "${skipped[@]}"; do summary+="- $t\n"; done
fi

if [[ -n "$summary" ]]; then
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
