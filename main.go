package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"context"
	"io/ioutil"
	"bufio"
	"time"
	"encoding/base64"
	"encoding/json"

	"github.com/asticode/go-astits"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
)

func readOffer(offerFile string) webrtc.SessionDescription {
	offerBuf, err := ioutil.ReadFile(offerFile)
	if err != nil {
		panic(err)
	}
	offer := webrtc.SessionDescription{}
	offerJson, err := base64.StdEncoding.DecodeString(string(offerBuf))
	if err != nil {
		panic(err)
	}
	err = json.Unmarshal(offerJson, &offer)
	if err != nil {
		panic(err)
	}
	return offer
}

func preparePeerConnection(peerConnection *webrtc.PeerConnection, demuxContextCancel context.CancelFunc, offer webrtc.SessionDescription) context.Context {
	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		log.Printf("Connection State has changed %s \n", connectionState.String())
		if connectionState == webrtc.ICEConnectionStateConnected {
			iceConnectedCtxCancel()
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Printf("Peer Connection State has changed: %s\n", s.String())
		if s == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			log.Println("Peer Connection has gone to failed exiting")
			demuxContextCancel()
		}
		if s == webrtc.PeerConnectionStateDisconnected {
			demuxContextCancel()
		}
	})
	
	// Set the remote SessionDescription
	if err := peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	// Create answer
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(peerConnection)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete
	log.Println("gather complete")

	// put answer to stdout
	answerJson, err := json.Marshal(peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}
	log.Println("-- answer -->")
	fmt.Println(base64.StdEncoding.EncodeToString(answerJson))
	log.Println("<-- answer --")

	return iceConnectedCtx
}

func addTrack(peerConnection *webrtc.PeerConnection, mimeType string, streamID string) (*webrtc.TrackLocalStaticSample) {
	track, err := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType:mimeType}, streamID, "pion")
	if err != nil {
		panic(err)
	}
	sender, err := peerConnection.AddTrack(track)
	if err != nil {
		panic(err)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := sender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()
	return track
}

type trkCtx struct {
	PID uint16
	PES *astits.PESData
	track *webrtc.TrackLocalStaticSample
}

func getDTS(PES *astits.PESData) int64 {
	if PES == nil || PES.Header == nil || PES.Header.OptionalHeader == nil {
		return -1
	}
	if PES.Header.OptionalHeader.DTS != nil {
		return PES.Header.OptionalHeader.DTS.Base
	}
	if PES.Header.OptionalHeader.PTS != nil {
		return PES.Header.OptionalHeader.PTS.Base
	}
	return -1
}

func PushVid(ctx *trkCtx, nextPES *astits.PESData) {
	DTS := getDTS(ctx.PES)
	nextDTS := getDTS(nextPES)
	diff := (nextDTS - DTS) & 0x1ffffffff
	log.Printf("vidPES %d %d %d\n", len(ctx.PES.Data), DTS, diff)
	if err := ctx.track.WriteSample(media.Sample{Data:ctx.PES.Data, Duration:time.Duration(diff * 1e9 / 90000)}); err != nil {
		panic(err)
	}
}

func PushAud(ctx *trkCtx, nextPES *astits.PESData) {
	DTS := getDTS(ctx.PES)
	nextDTS := getDTS(nextPES)
	diff := (nextDTS - DTS) & 0x1ffffffff
	log.Printf("audPES %d %d, %d\n", len(ctx.PES.Data), DTS, diff)
	poss := []int{}
	buflen := len(ctx.PES.Data)
	for pos := 0; pos + 2 < buflen; {
		control_header_prefix := (int(ctx.PES.Data[pos]) << 3) | int(ctx.PES.Data[pos + 1] >> 5)
		if control_header_prefix != 1023 {
			// panic("no sync")
			pos += 1
			continue
		}
		start_trim_flag := (ctx.PES.Data[pos + 1] >> 4) & 1
		end_trim_flag := (ctx.PES.Data[pos + 1] >> 3) & 1
		control_extension_flag := (ctx.PES.Data[pos + 1] >> 2) & 1
		payload_size := 0
		for pos += 2; pos < buflen; pos++ {
			size := int(ctx.PES.Data[pos])
			payload_size += size
			if size != 255 {
				pos++
				break
			}
		}
		if start_trim_flag == 1 {
			pos += 2
		}
		if end_trim_flag == 1 {
			pos += 2
		}
		if control_extension_flag == 1 {
			if pos >= buflen {
				break
			}
			control_extension_length := int(ctx.PES.Data[pos])
			pos += 1 + control_extension_length
		}
		if pos + payload_size > buflen {
			break
		}
		poss = append(poss, pos)
		poss = append(poss, pos + payload_size)
		pos += payload_size
	}
	numBufs := len(poss) / 2
	if numBufs <= 0 {
		return
	}
	bufDur := diff / int64(numBufs)
	for i := 0; i < numBufs; i++ {
		buf := ctx.PES.Data[poss[i * 2]:poss[i * 2 + 1]]
		var dur int64;
		if i + 1 < numBufs {
			dur = bufDur
			diff -= bufDur
		} else {
			dur = diff
		}
		if err := ctx.track.WriteSample(media.Sample{Data:buf, Duration:time.Duration(dur * 1e9 / 90000)}); err != nil {
			panic(err)
		}
	}
}

func main() {
	offerFile := flag.String("offerFile", "", "path to file which contains base64 offer string")
	stunURL := flag.String("stun", "stun:stun.1.google.com:19302", "STUN server URL")
	flag.Parse()
	offer := readOffer(*offerFile)

	demuxCtx, demuxCtxCancel := context.WithCancel(context.Background())
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-ch
		log.Println(sig)
		demuxCtxCancel()
		os.Exit(0)
	}()

	var vid *trkCtx
	var aud *trkCtx
	var peerConnection *webrtc.PeerConnection
	var ready = false
	stdin := bufio.NewReader(os.Stdin)
	demux := astits.NewDemuxer(demuxCtx, stdin)
	for {
		d, err := demux.NextData()
		if (err != nil) {
			log.Printf("err: \n", err);
			break;
		}
		// log.Printf("%+v\n", d)

		switch {
		case peerConnection == nil && d.PMT != nil:
			URLs := []string{}
			if *stunURL != "" {
				URLs = append(URLs, *stunURL)
			}
			// Create a new RTCPeerConnection
			peerConnection, err = webrtc.NewPeerConnection(webrtc.Configuration{
				ICEServers: []webrtc.ICEServer{
					{
						URLs: URLs,
					},
				},
			})
			if err != nil {
				panic(err)
			}
			defer func() {
				if cErr := peerConnection.Close(); cErr != nil {
					log.Printf("cannot close peerConnection: %v\n", cErr);
				}
			}()
			for _, es := range d.PMT.ElementaryStreams {
				switch {
				case vid == nil && es.StreamType == astits.StreamTypeH264Video:
					vid = &trkCtx{
						PID: es.ElementaryPID,
						track: addTrack(peerConnection, webrtc.MimeTypeH264, "video"),
					}
					log.Printf("H264 video: %d\n", vid.PID)
				case aud == nil && es.StreamType == astits.StreamTypePrivateData:
					for _, descr := range es.ElementaryStreamDescriptors {
						switch {
						case descr.Registration != nil && descr.Registration.FormatIdentifier == 0x4F707573: // Opus
							aud = &trkCtx{
								PID: es.ElementaryPID,
								track: addTrack(peerConnection, webrtc.MimeTypeOpus, "audio"),
							}
							log.Printf("Opus audio: %d\n", aud.PID)
						}
					}
				}
			}
			go func() {
				iceConnectedCtx := preparePeerConnection(peerConnection, demuxCtxCancel, offer)
				// Wait for connection established
				<-iceConnectedCtx.Done()
				ready = true
				log.Println("connection established")
			}()
		case vid != nil && vid.PID == d.PID:
			if vid.PES != nil && ready {
				PushVid(vid, d.PES)
			}
			vid.PES = d.PES
		case aud != nil && aud.PID == d.PID:
			if aud.PES != nil && ready {
				PushAud(aud, d.PES)
			}
			aud.PES = d.PES
		}
	}
	log.Println("done")
}
