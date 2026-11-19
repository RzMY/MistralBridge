"""Create a GitHub release draft after verifying every platform package."""

import json
import os
from pathlib import Path
import subprocess

from artifacts import checksum_content, release_version


def gh(*args):
    return subprocess.check_output(["gh", *args], text=True, encoding="utf-8").strip()


def ensure_draft(tag, commit, assets):
    # Listing includes drafts and supports reruns after a partial asset upload.
    pages = json.loads(gh("api", "--paginate", "--slurp", f"repos/{os.environ['GH_REPO']}/releases?per_page=100"))
    existing = next((release for page in pages for release in page if release["tag_name"] == tag), None)
    if existing:
        if not existing["draft"]:
            raise RuntimeError(f"{tag} is already published; refusing to replace its assets")
        gh("release", "upload", tag, *assets, "--clobber")
    else:
        gh("release", "create", tag, *assets, "--draft", "--verify-tag",
           "--target", commit, "--title", f"Mistral Bridge {tag}", "--generate-notes")
    result = json.loads(gh("release", "view", tag, "--json", "isDraft,url,assets"))
    expected = {Path(asset).name: Path(asset).stat().st_size for asset in assets}
    attached = {asset["name"]: asset["size"] for asset in result["assets"]}
    if not result["isDraft"] or any(attached.get(name) != size for name, size in expected.items()):
        raise RuntimeError("Release draft or attached assets did not pass verification")
    return result["url"]


def main():
    if os.environ.get("GITHUB_REF_TYPE") != "tag":
        raise RuntimeError("Release drafts are only created for tag pushes")
    tag = os.environ["GITHUB_REF_NAME"]
    version = release_version(tag)
    output = Path("dist")
    expected = checksum_content(version, output)
    if (output / "checksums.txt").read_text(encoding="utf-8") != expected:
        raise RuntimeError("Artifact checksums do not match checksums.txt")
    # Resolve annotated tags to their commit rather than using the tag object ID.
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    assets = [str(path) for path in sorted(output.glob("*.zip"))] + [str(output / "checksums.txt")]
    url = ensure_draft(tag, commit, assets)
    print(f"Release draft ready: {url}")
    with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as summary:
        summary.write(f"[Review release draft {tag}]({url})\n\n")
        summary.write("\n".join(f"- `{Path(asset).name}`" for asset in assets) + "\n")


if __name__ == "__main__":
    main()
