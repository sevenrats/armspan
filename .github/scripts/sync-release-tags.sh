#!/usr/bin/env bash
# sync-release-tags.sh — for each upstream semver tag that lacks a
# corresponding *-armspan tag, overlay the armspan files and push.
#
# Strategy (conflict-free):
#   1. Checkout the upstream release tag
#   2. Copy all NEW files from main-armspan (git diff --diff-filter=A)
#   3. sed-inject the one-line hook into hscontrol/db/db.go
#   4. Commit, tag as {version}-armspan, push
#
# No cherry-pick or rebase — upstream files are never modified except
# db.go via sed, so there are zero merge conflicts.
#
# Outputs:
#   GITHUB_OUTPUT: summary=<markdown table of results>

set -euo pipefail

# ── Configuration ────────────────────────────────────────────────────────
ARMSPAN_MIN_TAG="${ARMSPAN_MIN_TAG:-v0.24.0}"
UPSTREAM_URL="${UPSTREAM_URL:-https://github.com/juanfont/headscale.git}"
GITHUB_OUTPUT="${GITHUB_OUTPUT:-/dev/null}"

# The sed pattern and replacement for the db.go hook injection.
# Anchor: the closing "}" of the openDB error-check block.
# We match: a line with "return nil, err" followed by a line with just "}",
# and append the hook call after it.
HOOK_LINE=$'\tpostOpenDBHook(dbConn) // armspan: plugin hook (see gormspan_init.go)'
DB_GO="hscontrol/db/db.go"

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
    # Find the openDB call and its error block, insert hook after the closing }
    # Pattern: line matching "return nil, err" inside the openDB error check,
    # followed by a line with just whitespace + "}"
    if grep -qP '^\s+dbConn, err := openDB' "$target"; then
      # Use sed: after the "return nil, err" + "}" block following openDB, insert hook
      sed -i '/dbConn, err := openDB/{
        # Read lines until we find the closing } of the error check
        n; # if err != nil {
        n; # return nil, err
        n; # }
        a\
\tpostOpenDBHook(dbConn) // armspan: plugin hook (see gormspan_init.go)
      }' "$target"

      # Verify it was inserted
      grep -q 'postOpenDBHook' "$target"
    else
      return 1
    fi
  fi
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

# Already-existing armspan tags
declare -A existing_armspan
for t in $(git tag -l '*-armspan'); do
  existing_armspan["$t"]=1
done

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
  armspan_tag="${tag}-armspan"

  if ! version_ge "$tag" "$ARMSPAN_MIN_TAG"; then
    skipped+=("$tag (below minimum $ARMSPAN_MIN_TAG)")
    continue
  fi

  if [[ -n "${existing_armspan[$armspan_tag]:-}" ]]; then
    skipped+=("$tag (already exists)")
    continue
  fi

  log "Processing $tag → $armspan_tag"

  # 1. Start from the release tag.
  tmp_branch="tmp-armspan-${tag}"
  git branch -D "$tmp_branch" 2>/dev/null || true
  git checkout "$tag" 2>/dev/null
  git checkout -b "$tmp_branch" 2>/dev/null

  # 2. Copy armspan-only NEW files from main-armspan onto this tag.
  #    We identify new files as those added in the armspan delta
  #    (status=A in diff) — never modified upstream files.
  mapfile -t new_files < <(
    git diff --name-only --diff-filter=A "$armspan_base" main-armspan
  )

  if [[ ${#new_files[@]} -gt 0 ]]; then
    git checkout main-armspan -- "${new_files[@]}"
    git add "${new_files[@]}"
    git commit -m "armspan: add gormspan plugin and CI infrastructure" --no-verify
  fi

  # 3. Inject the hook into db.go via sed.
  if ! inject_hook "$DB_GO"; then
    git checkout main-armspan 2>/dev/null
    git branch -D "$tmp_branch" 2>/dev/null || true
    failed+=("$tag (db.go anchor not found — upstream changed)")
    echo "❌ Failed $armspan_tag (sed: openDB anchor missing)"
    endg
    continue
  fi

  # 4. Commit the db.go hook, tag, and push.
  git add "$DB_GO"
  git commit -m "armspan: hook gormspan into db.go" --no-verify

  git tag -d "$armspan_tag" 2>/dev/null || true
  git tag "$armspan_tag"
  git push github "refs/tags/$armspan_tag"
  created+=("$tag")
  echo "✅ Created $armspan_tag"

  git checkout main-armspan 2>/dev/null
  git branch -D "$tmp_branch" 2>/dev/null || true

  endg
done

# Return to main-armspan
git checkout main-armspan 2>/dev/null || true

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
