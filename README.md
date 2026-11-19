# TinyMikuServer
 Server for streaming data to tiny Miku Holocube AI Assistant

## HoloCropper

Content-aware video cropper used to prepare videos for the tiny Miku holocube displays: `pkg/holocropper` (Go package) with a CLI at `cmd/holocropper`, built to `bin/holocropper` via `make build-holocropper`.

```sh
bin/holocropper -i vids/song.mp4 -o out-dir -w 200 -h 648 -fps 24
bin/holocropper -i vids/song.mp4 -o out-dir -w 648 -h 200 -fps 24 -auto-orient
```

Any output aspect ratio (1:1, portrait `200x648`, landscape `648x200`), optional auto-orient probing, walk mode for landscape-to-landscape framing, and a PID framing controller. Full documentation: [pkg/holocropper/README.md](pkg/holocropper/README.md). Design and measured results: [HIGH-ASPECT-RATIO-FIXES.md](HIGH-ASPECT-RATIO-FIXES.md).
