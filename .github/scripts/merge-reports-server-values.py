#!/usr/bin/env python3
"""
Merge new keys from upstream reports-server values.yaml into enterprise
values.yaml under the 'reports-server' key. Preserves comments and formatting.
Only adds keys that don't exist (deep merge); never overwrites existing values.
"""
import sys
from ruamel.yaml import YAML

yaml = YAML()
yaml.preserve_quotes = True
yaml.width = 4096


def deep_merge_add_only(target, source, path=""):
    """Add keys from source into target only if absent. Returns list of added key paths."""
    added = []
    for key in source:
        if key not in target:
            target[key] = source[key]
            if hasattr(source, "ca") and key in source.ca.items:
                target.ca.items[key] = source.ca.items[key]
            added.append(f"{path}.{key}" if path else str(key))
        elif isinstance(target[key], dict) and isinstance(source[key], dict):
            added.extend(
                deep_merge_add_only(
                    target[key], source[key], f"{path}.{key}" if path else str(key)
                )
            )
    return added


def main():
    if len(sys.argv) != 4:
        print(
            "Usage: merge-reports-server-values.py <upstream> <enterprise> <output>",
            file=sys.stderr,
        )
        sys.exit(1)

    upstream_path, enterprise_path, output_path = sys.argv[1], sys.argv[2], sys.argv[3]

    with open(upstream_path) as f:
        upstream = yaml.load(f)
    with open(enterprise_path) as f:
        enterprise = yaml.load(f)

    if upstream is None:
        print("Upstream values.yaml is empty — nothing to merge")
        sys.exit(0)

    if "reports-server" not in enterprise:
        enterprise["reports-server"] = upstream
        print("Created reports-server section from upstream")
    else:
        added = deep_merge_add_only(enterprise["reports-server"], upstream)
        if added:
            print(f"Added {len(added)} new key(s): {', '.join(added)}")
        else:
            print("No new keys to add")

    with open(output_path, "w") as f:
        yaml.dump(enterprise, f)
    print(f"Wrote merged values to {output_path}")


if __name__ == "__main__":
    main()
