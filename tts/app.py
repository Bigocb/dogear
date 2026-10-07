#!/usr/bin/env python3
"""dogear-tts: a tiny HTTP wrapper around the Piper neural TTS binary.

Endpoints
  GET /health             -> {"ok": true, "voices": n}
  GET /voices             -> {"voices": [{id,name,lang,quality,source}]}
  GET /tts?text=&voice=&length_scale=  -> audio/wav

No third-party Python deps: stdlib http.server + the piper subprocess.
"""
import json
import os
import subprocess
import sys
import tempfile
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PIPER_BIN = os.environ.get("PIPER_BIN", "/opt/piper/piper")
VOICES_DIR = os.environ.get("VOICES_DIR", "/voices")
PORT = int(os.environ.get("PORT", "8097"))
MAX_CHARS = int(os.environ.get("MAX_CHARS", "2000"))

# piper loads a voice per call; serialise invocations so a 4-core box stays
# responsive and two requests never fight over CPU.
_lock = threading.Lock()


def discover_voices():
    out = []
    if not os.path.isdir(VOICES_DIR):
        return out
    for name in sorted(os.listdir(VOICES_DIR)):
        if not name.endswith(".onnx"):
            continue
        base = name[:-5]
        meta_path = os.path.join(VOICES_DIR, name + ".json")
        lang, quality, speakers = "", "", 1
        dataset = ""
        try:
            with open(meta_path) as f:
                meta = json.load(f)
            lang = meta.get("language", {}).get("code", "")
            quality = meta.get("audio", {}).get("quality", "")
            dataset = meta.get("dataset", "")
            speakers = int(meta.get("num_speakers", 1) or 1)
        except Exception:
            pass
        out.append({
            "id": base,
            "name": voice_label(base, dataset, quality),
            "lang": lang,
            "quality": quality,
            "speakers": speakers,
            "source": "piper",
        })
    return out


def voice_label(base, dataset, quality):
    """Human-friendly label from the voice id, e.g. en_US-lessac-medium."""
    parts = base.split("-")
    ds = dataset or (parts[1] if len(parts) > 1 else base)
    q = quality or (parts[2] if len(parts) > 2 else "")
    nice_ds = ds.replace("_", " ").title() if len(ds) <= 6 else ds
    return f"{nice_ds} ({q})" if q else nice_ds


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # keep the container log quiet
        pass

    def _json(self, code, obj):
        body = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        u = urlparse(self.path)
        if u.path == "/health":
            return self._json(200, {"ok": True, "voices": len(discover_voices())})
        if u.path == "/voices":
            return self._json(200, {"voices": discover_voices()})
        if u.path == "/tts":
            return self._tts(parse_qs(u.query))
        self._json(404, {"error": "not found"})

    def _tts(self, q):
        text = (q.get("text", [""])[0] or "").strip()[:MAX_CHARS]
        if not text:
            return self._json(400, {"error": "text required"})
        voice = (q.get("voice", [""])[0] or "").strip()
        model = self._resolve_model(voice)
        if not model:
            return self._json(404, {"error": f"unknown voice: {voice}"})
        try:
            length_scale = float(q.get("length_scale", ["1"])[0])
        except ValueError:
            length_scale = 1.0
        length_scale = min(max(length_scale, 0.5), 2.5)

        try:
            wav = self._synthesize(text, model, length_scale)
        except subprocess.CalledProcessError as e:
            return self._json(502, {"error": (e.stderr or b"piper failed").decode(errors="replace")[:400]})
        except Exception as e:  # noqa: BLE001
            return self._json(500, {"error": str(e)})

        self.send_response(200)
        self.send_header("Content-Type", "audio/wav")
        self.send_header("Content-Length", str(len(wav)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(wav)

    def _resolve_model(self, voice):
        if not voice:
            vs = discover_voices()
            voice = vs[0]["id"] if vs else ""
        if not voice or "/" in voice or ".." in voice:
            return ""
        path = os.path.join(VOICES_DIR, voice + ".onnx")
        return path if os.path.isfile(path) else ""

    def _synthesize(self, text, model, length_scale):
        with _lock:
            with tempfile.NamedTemporaryFile(suffix=".wav", delete=False) as tmp:
                out = tmp.name
            try:
                cmd = [PIPER_BIN, "--model", model, "--output_file", out,
                       "--length_scale", str(length_scale)]
                subprocess.run(cmd, input=text.encode(), check=True,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
                with open(out, "rb") as f:
                    return f.read()
            finally:
                try:
                    os.unlink(out)
                except OSError:
                    pass


def main():
    print(f"dogear-tts on :{PORT} (piper={PIPER_BIN} voices={VOICES_DIR})", flush=True)
    print(f"voices: {[v['id'] for v in discover_voices()]}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
