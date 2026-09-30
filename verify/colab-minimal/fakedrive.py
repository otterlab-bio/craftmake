#!/usr/bin/env python3
"""Minimal Google Drive v3 emulator for the local demo harness.

It implements just the endpoints the Drive REST transport uses, so the transport
can be verified end to end without touching the real API (and without being
subject to the shared rclone client's quota).

    python3 fakedrive.py --port 0

Prints the bound port on stdout, then serves:
  GET    /drive/v3/files?q=...              list (subset of the query language)
  POST   /drive/v3/files                    create a folder
  POST   /upload/drive/v3/files?uploadType=multipart   create a file
  PATCH  /upload/drive/v3/files/<id>?uploadType=media  replace content
  GET    /drive/v3/files/<id>?alt=media     download
  GET    /__state                           JSON state for test assertions
"""

import argparse
import json
import re
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

FOLDER_MIME = "application/vnd.google-apps.folder"


class Drive:
    def __init__(self):
        self.lock = threading.Lock()
        self.next_id = 0
        self.entries = {"root": {"id": "root", "name": "root", "parent": None, "mime": FOLDER_MIME, "content": b""}}
        self.requests = []

    def new_id(self):
        self.next_id += 1
        return "id-%d" % self.next_id

    def children(self, parent, name=None, folders_only=False):
        out = []
        for entry in self.entries.values():
            if entry["parent"] != parent:
                continue
            if name is not None and entry["name"] != name:
                continue
            if folders_only and entry["mime"] != FOLDER_MIME:
                continue
            out.append(entry)
        return out

    def path_of(self, entry):
        parts = [entry["name"]]
        parent = entry["parent"]
        while parent and parent != "root":
            node = self.entries.get(parent)
            if not node:
                break
            parts.append(node["name"])
            parent = node["parent"]
        return "/".join(reversed(parts))

    def state(self):
        files = {}
        for entry in self.entries.values():
            if entry["mime"] != FOLDER_MIME and entry["parent"] is not None:
                files[self.path_of(entry)] = entry["content"].decode("utf-8", "replace")
        return {"files": files, "requests": self.requests}


def parse_query(query):
    parent = None
    name = None
    match = re.search(r"name\s*=\s*'([^']*)'", query)
    if match:
        name = match.group(1)
    match = re.search(r"'([^']*)'\s+in\s+parents", query)
    if match:
        parent = match.group(1)
    return parent, name, FOLDER_MIME in query


class Handler(BaseHTTPRequestHandler):
    drive = None

    def log_message(self, *args):
        pass

    def _json(self, payload, status=200):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _parse_multipart(self):
        content_type = self.headers.get("Content-Type", "")
        match = re.search(r"boundary=([^;]+)", content_type)
        if not match:
            return None, b""
        boundary = match.group(1).strip('"').encode()
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        metadata = None
        content = b""
        for part in raw.split(b"--" + boundary):
            if not part.strip() or part.strip() == b"--":
                continue
            head, _, body = part.partition(b"\r\n\r\n")
            body = body.rstrip(b"\r\n")
            if b"application/json" in head:
                try:
                    metadata = json.loads(body.decode())
                except Exception:
                    metadata = None
            else:
                content = body
        return metadata, content

    def do_GET(self):
        drive = Handler.drive
        if self.path.startswith("/__state"):
            self._json(drive.state())
            return
        path = self.path.split("?")[0]
        if path == "/drive/v3/files":
            from urllib.parse import parse_qs, urlparse

            query = parse_qs(urlparse(self.path).query).get("q", [""])[0]
            parent, name, folders_only = parse_query(query)
            with drive.lock:
                drive.requests.append(("list", parent, name))
                files = [{"id": e["id"], "name": e["name"], "mimeType": e["mime"]} for e in drive.children(parent, name, folders_only)]
            self._json({"files": files})
            return
        match = re.match(r"^/drive/v3/files/([^/]+)$", path)
        if match:
            entry = drive.entries.get(match.group(1))
            if not entry:
                self._json({"error": {"code": 404}}, 404)
                return
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(len(entry["content"])))
            self.end_headers()
            self.wfile.write(entry["content"])
            return
        self._json({"error": {"code": 404}}, 404)

    def do_POST(self):
        drive = Handler.drive
        path = self.path.split("?")[0]
        if path == "/drive/v3/files":
            payload = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
            parent = (payload.get("parents") or ["root"])[0]
            with drive.lock:
                entry = {"id": drive.new_id(), "name": payload.get("name", ""), "parent": parent,
                         "mime": payload.get("mimeType", "application/octet-stream"), "content": b""}
                drive.entries[entry["id"]] = entry
                drive.requests.append(("create-folder", parent, entry["name"]))
            self._json({"id": entry["id"]})
            return
        if path == "/upload/drive/v3/files":
            metadata, content = self._parse_multipart()
            if metadata is None:
                self._json({"error": {"code": 400, "message": "bad multipart"}}, 400)
                return
            parent = (metadata.get("parents") or ["root"])[0]
            with drive.lock:
                entry = {"id": drive.new_id(), "name": metadata.get("name", ""), "parent": parent,
                         "mime": "application/octet-stream", "content": content}
                drive.entries[entry["id"]] = entry
                drive.requests.append(("create-file", parent, entry["name"], len(content)))
            self._json({"id": entry["id"]})
            return
        self._json({"error": {"code": 404}}, 404)

    def do_PATCH(self):
        drive = Handler.drive
        match = re.match(r"^/upload/drive/v3/files/([^/]+)$", self.path.split("?")[0])
        if not match:
            self._json({"error": {"code": 404}}, 404)
            return
        entry = drive.entries.get(match.group(1))
        if not entry:
            self._json({"error": {"code": 404}}, 404)
            return
        entry["content"] = self.rfile.read(int(self.headers.get("Content-Length", "0")))
        with drive.lock:
            drive.requests.append(("update-file", entry["name"], len(entry["content"])))
        self._json({"id": entry["id"]})


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--port", type=int, default=0)
    args = parser.parse_args()
    Handler.drive = Drive()
    server = ThreadingHTTPServer(("127.0.0.1", args.port), Handler)
    print(server.server_address[1], flush=True)
    server.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
