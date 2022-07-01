# mpegts-to-webrtc
[![Go Report](https://goreportcard.com/badge/github.com/wakabayashik/mpegts-to-webrtc)](https://goreportcard.com/report/github.com/wakabayashik/mpegts-to-webrtc)

demux mpegts to H264 and Opus then send to a WebRTC client using [pion](https://github.com/pion/webrtc).

* offer from file, mpegts from stdin
    * read base64 offer from file (needed -offerFile)
    * read mpegts stream from stdin

```
ffmpeg -i {{src}} -c:v copy -c:a libopus -ar 48000 -ac 2 -f mpegts -pes_payload_size 0 pipe:1 | mpegts-to-webrtc -offerFile {{offer-file-path}}
```

* offer from stdin, mpegts from ffmpeg
    * read base64 offer from stdin
    * launch ffmpeg as child process and read stdout (needed -ffmpeg)

```
mpegts-to-webrtc -ffmpeg ffmpeg -i {{src}} -c:v copy -c:a libopus -ar 48000 -ac 2 -f mpegts -pes_payload_size 0 pipe:1 < {{offer-file-path}}
```
