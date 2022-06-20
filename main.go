package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"io/ioutil"
	"flag"
	"bufio"
	"context"
	"syscall"
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

func main() {
	offerFile := flag.String("offerFile", "", "path to file which contains base64 offer string")
	h264Duration := flag.Int("h264Duration", 33, "H264 sample duration in milliseconds")
	opusDuration := flag.Int("opusDuration", 20, "Opus AAU duration in milliseconds")
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

	var vidPid uint16 = 0xffff
	var audPid uint16 = 0xffff
	var peerConnection *webrtc.PeerConnection
	var vidTrack *webrtc.TrackLocalStaticSample
	var audTrack *webrtc.TrackLocalStaticSample
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
			// Create a new RTCPeerConnection
			peerConnection, err = webrtc.NewPeerConnection(webrtc.Configuration{
				ICEServers: []webrtc.ICEServer{
					{
						URLs: []string{"stun:stun.1.google.com:19302"},
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
			var vidEs *astits.PMTElementaryStream
			var audEs *astits.PMTElementaryStream
			for _, es := range d.PMT.ElementaryStreams {
				switch {
				case vidPid == 0xffff && es.StreamType == astits.StreamTypeH264Video:
					vidEs = es
					vidTrack = addTrack(peerConnection, webrtc.MimeTypeH264, "video")
					log.Printf("H264 video: %d\n", vidEs.ElementaryPID)
				// case audPid == 0xffff && es.StreamType == astits.StreamTypeMPEG1Audio:
				// 	audEs = es
				// 	log.Printf("mpeg1 audio: %d\n", audEs.ElementaryPID)
				// case audPid == 0xffff && es.StreamType == astits.StreamTypeADTS:
				// 	audEs = es
				// 	log.Printf("ADTS audio: %d\n", audEs.ElementaryPID)
				case audPid == 0xffff && es.StreamType == astits.StreamTypePrivateData:
					for _, descr := range es.ElementaryStreamDescriptors {
						switch {
						// case descr.Extension != nil:
						// 	log.Printf("Extension: %+v\n", descr.Extension)
						case descr.Registration != nil && descr.Registration.FormatIdentifier == 0x4F707573: // Opus
							audEs = es
							audTrack = addTrack(peerConnection, webrtc.MimeTypeOpus, "audio")
							log.Printf("Opus audio: %d\n", audEs.ElementaryPID)
						}
					}
				}
			}
			go func() {
				iceConnectedCtx := preparePeerConnection(peerConnection, demuxCtxCancel, offer)
				// Wait for connection established
				<-iceConnectedCtx.Done()
				log.Println("connection established")
				if vidEs != nil {
					vidPid = vidEs.ElementaryPID
				}
				if audEs != nil {
					audPid = audEs.ElementaryPID
				}
			}()
		case d.PID == vidPid:
			log.Printf("video %d\n", len(d.PES.Data))
			if err := vidTrack.WriteSample(media.Sample{Data:d.PES.Data, Duration:time.Millisecond * time.Duration(*h264Duration)}); err != nil {
				panic(err)
			}
		case d.PID == audPid:
			pos := 0
			buflen := len(d.PES.Data)
			for ; pos + 2 < buflen; {
				control_header_prefix := (int(d.PES.Data[pos]) << 3) | int(d.PES.Data[pos + 1] >> 5)
				if control_header_prefix != 1023 {
					// panic("no sync")
					pos += 1
					continue
				}
				start_trim_flag := (d.PES.Data[pos + 1] >> 4) & 1
				end_trim_flag := (d.PES.Data[pos + 1] >> 3) & 1
				control_extension_flag := (d.PES.Data[pos + 1] >> 2) & 1
				payload_size := 0
				for pos += 2; pos < buflen; pos++ {
					size := int(d.PES.Data[pos])
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
					control_extension_length := int(d.PES.Data[pos])
					pos += 1 + control_extension_length
				}
				if pos + payload_size > buflen {
					break
				}
				buf := d.PES.Data[pos:pos + payload_size]
				log.Printf("audio %d\n", len(buf))
				if err := audTrack.WriteSample(media.Sample{Data:buf, Duration:time.Millisecond * time.Duration(*opusDuration)}); err != nil {
					panic(err)
				}
				pos += payload_size
			}
		}
	}
	log.Println("done")
}
