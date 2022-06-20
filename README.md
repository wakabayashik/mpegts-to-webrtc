# mpegts-to-webrtc
demux mpegts to H264 and Opus then send to a WebRTC client.

```
ffmpeg -y -i {{src}} -c:v copy -c:a libopus -ar 48000 -ac 2 -f mpegts pipe:1 | mpegts-to-webrtc -offerFile {{offer-file-path}}
```