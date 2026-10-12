"""Publish binaries to GitLab's package registry and create/update a release."""

import json
import os
from pathlib import Path
from urllib.error import HTTPError
from urllib.parse import quote
from urllib.request import Request, urlopen


def main():
    project_api = f"{os.environ['CI_API_V4_URL']}/projects/{os.environ['CI_PROJECT_ID']}"
    tag = os.environ["CI_COMMIT_TAG"]
    headers = {"JOB-TOKEN": os.environ["CI_JOB_TOKEN"]}

    def request(method, url, data=None, content_type="application/json"):
        req = Request(
            url,
            data=data,
            headers={**headers, "Content-Type": content_type},
            method=method,
        )
        with urlopen(req, timeout=300) as response:
            return response.read()

    binaries = [
        Path("build") / f"lrcAPI-{goos}-{arch}{'.exe' if goos == 'windows' else ''}"
        for goos in ("linux", "windows", "darwin")
        for arch in ("amd64", "arm64")
    ]
    missing = [str(path) for path in binaries if not path.is_file()]
    if missing:
        raise SystemExit(f"Missing build artifacts: {', '.join(missing)}")

    links = []
    for binary in binaries:
        url = f"{project_api}/packages/generic/lrcapi/{quote(tag, safe='')}/{binary.name}"
        request("PUT", url, binary.read_bytes(), "application/octet-stream")
        links.append({"name": binary.name, "url": url, "link_type": "package"})
        print(f"Uploaded {binary.name}")

    release_url = f"{project_api}/releases/{quote(tag, safe='')}"
    payload = {
        "name": tag,
        "description": f"lrcAPI {tag}",
        "assets": {"links": links},
    }
    try:
        request("GET", release_url)
    except HTTPError as error:
        if error.code != 404:
            raise
        payload["tag_name"] = tag
        request("POST", f"{project_api}/releases", json.dumps(payload).encode())
    else:
        # The release update endpoint does not update asset links. Keep its
        # existing links: package URLs remain the same when retrying this job.
        payload.pop("assets")
        request("PUT", release_url, json.dumps(payload).encode())
    print(f"Published release {tag}")


if __name__ == "__main__":
    main()
