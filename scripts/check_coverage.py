"""Enforce the documented coverage floor for the Kiro business package."""
import sys
from pathlib import Path

covered = total = 0
for line in Path(sys.argv[1]).read_text().splitlines()[1:]:
    location, statements, hits = line.split()
    if '/internal/kiro/' not in location:
        continue
    total += int(statements)
    if int(hits) > 0:
        covered += int(statements)
ratio = 100 * covered / total if total else 0
print(f'internal/kiro coverage: {ratio:.1f}% (required: 85%)')
if not total or ratio < 85:
    sys.exit(1)
