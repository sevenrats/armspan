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

# ── Helpers ──────────────────────────────────────────────────────────────
semver_re='^v[0-9]+\.[0-9]+\.[0-9]+$'

log()  { echo "::group::$*"; }
endg() { echo "::endgroup::"; }

# ── Gather tags ──────────────────────────────────────────────────────────
mapfile -t upstream_tags < <(
  git tag -l 'v*' --sort=-v:refname |
    grep -E "$semver_re" || true
)

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
