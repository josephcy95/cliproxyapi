#!/usr/bin/env bash
set -euo pipefail

models_repository="${MODELS_REPOSITORY_URL:-https://github.com/router-for-me/models.git}"
models_ref="${MODELS_REPOSITORY_REF:-main}"
catalog_dir="${MODEL_CATALOG_DIR:-internal/registry/models}"
codex_catalog="$catalog_dir/codex_client_models.json"
models_candidate="$(mktemp)"
codex_candidate="$(mktemp)"
trap 'rm -f "$models_candidate" "$codex_candidate"' EXIT

git fetch --depth 1 "$models_repository" "$models_ref"
git show FETCH_HEAD:models.json > "$models_candidate"
# Upstream does not publish the fork's Qoder catalogs. Keep the checked-in
# fallback when the downloaded section is absent or empty; accept live updates.
python3 - "$catalog_dir/models.json" "$models_candidate" <<'PYTHON'
import json
import sys

baseline_path, candidate_path = sys.argv[1:]
with open(baseline_path, encoding="utf-8") as source:
    baseline = json.load(source)
with open(candidate_path, encoding="utf-8") as source:
    candidate = json.load(source)
if not isinstance(baseline, dict) or not isinstance(candidate, dict):
    raise ValueError("model catalog must be an object")
for provider in ("qodercn", "qoder"):
    models = candidate.get(provider)
    if models is not None and not isinstance(models, list):
        raise ValueError(f"{provider} model catalog must be an array")
    if not models:
        models = baseline.get(provider)
        if not isinstance(models, list) or not models:
            raise ValueError(f"{provider} embedded fallback is missing")
        candidate[provider] = models
with open(candidate_path, "w", encoding="utf-8") as output:
    json.dump(candidate, output, ensure_ascii=False, indent=2)
    output.write("\n")
PYTHON
mv "$models_candidate" "$catalog_dir/models.json"

if git show FETCH_HEAD:codex_client_models.json > "$codex_candidate" &&
  go run ./cmd/validate_codex_models --file "$codex_candidate"; then
	mv "$codex_candidate" "$codex_catalog"
	printf 'Refreshed validated Codex client model catalog.\n'
else
	printf '::warning::Remote Codex client model catalog is missing or invalid; using embedded fallback.\n'
fi

go run ./cmd/validate_codex_models --file "$codex_catalog"
