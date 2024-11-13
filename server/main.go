package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gortsplib/v4"
	"github.com/bluenviron/gortsplib/v4/pkg/base"
	"github.com/bluenviron/gortsplib/v4/pkg/format"
	"github.com/bluenviron/gortsplib/v4/pkg/format/rtph264"
	"github.com/deepch/vdk/av"
	"github.com/deepch/vdk/codec/h264parser"
	"github.com/deepch/vdk/format/rtsp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/shirou/gopsutil/cpu"
)

var (
	outboundVideoTrack  *webrtc.TrackLocalStaticSample
	peerConnectionCount int64
)

// Generate CSV with columns of timestamp, peerConnectionCount, and cpuUsage
func reportBuilder() {
	file, err := os.OpenFile("report.csv", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		panic(err)
	}

	if _, err := file.WriteString("timestamp, peerConnectionCount, cpuUsage\n"); err != nil {
		panic(err)
	}

	for range time.NewTicker(3 * time.Second).C {
		usage, err := cpu.Percent(0, false)
		if err != nil {
			panic(err)
		} else if len(usage) != 1 {
			panic(fmt.Sprintf("CPU Usage results should have 1 sample, have %d", len(usage)))
		}
		if _, err = file.WriteString(fmt.Sprintf("%s, %d, %f\n", time.Now().Format(time.RFC3339), atomic.LoadInt64(&peerConnectionCount), usage[0])); err != nil {
			panic(err)
		}
	}
}

// HTTP Handler that accepts an Offer and returns an Answer
// adds outboundVideoTrack to PeerConnection
func doSignaling(w http.ResponseWriter, r *http.Request) {
	peerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		panic(err)
	}

	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		if connectionState == webrtc.ICEConnectionStateDisconnected {
			atomic.AddInt64(&peerConnectionCount, -1)
			if err := peerConnection.Close(); err != nil {
				panic(err)
			}
		} else if connectionState == webrtc.ICEConnectionStateConnected {
			atomic.AddInt64(&peerConnectionCount, 1)
		}
	})

	if _, err = peerConnection.AddTrack(outboundVideoTrack); err != nil {
		panic(err)
	}

	var offer webrtc.SessionDescription
	if err = json.NewDecoder(r.Body).Decode(&offer); err != nil {
		panic(err)
	}

	if err = peerConnection.SetRemoteDescription(offer); err != nil {
		panic(err)
	}

	gatherCompletePromise := webrtc.GatheringCompletePromise(peerConnection)

	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		panic(err)
	} else if err = peerConnection.SetLocalDescription(answer); err != nil {
		panic(err)
	}

	<-gatherCompletePromise

	response, err := json.Marshal(*peerConnection.LocalDescription())
	if err != nil {
		panic(err)
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(response); err != nil {
		panic(err)
	}
}

func main() {
	// Decode()
	var err error
	outboundVideoTrack, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{
		MimeType: "video/h264",
	}, "pion-rtsp", "pion-rtsp")
	if err != nil {
		panic(err)
	}

	go rtspConsumer2()
	go reportBuilder()

	http.Handle("/", http.FileServer(http.Dir("./static")))
	http.HandleFunc("/doSignaling", doSignaling)

	fmt.Println("Open http://localhost:8080 to access this demo")
	panic(http.ListenAndServe(":8080", nil))
}

// The RTSP URL that will be streamed
// const rtspURL = "rtsp://onvifuser:onvifpassword1@10.3.0.10/Streaming/Channels/101?channel=1&profile=Profile_1&subtype=0&transportmode=unicast"
const rtspURL = "rtsp://10.0.0.104:554/cam/realmonitor?channel=1&subtype=0&unicast=true&proto=Onvif"

// Connect to an RTSP URL and pull media.
// Convert H264 to Annex-B, then write to outboundVideoTrack which sends to all PeerConnections
func rtspConsumer() {
	annexbNALUStartCode := func() []byte { return []byte{0x00, 0x00, 0x00, 0x01} }

	for {
		session, err := rtsp.Dial(rtspURL)
		if err != nil {
			panic(err)
		}
		session.RtpKeepAliveTimeout = 10 * time.Second

		codecs, err := session.Streams()
		if err != nil {
			panic(err)
		}
		for i, t := range codecs {
			log.Println("Stream", i, "is of type", t.Type().String())
		}
		if codecs[0].Type() != av.H264 {
			panic("RTSP feed must begin with a H264 codec")
		}
		if len(codecs) != 1 {
			log.Println("Ignoring all but the first stream.")
		}

		var previousTime time.Duration
		for {
			pkt, err := session.ReadPacket()
			if err != nil {
				break
			}

			if pkt.Idx != 0 {
				//audio or other stream, skip it
				continue
			}

			pkt.Data = pkt.Data[4:]

			// For every key-frame pre-pend the SPS and PPS
			if pkt.IsKeyFrame {
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
				pkt.Data = append(codecs[0].(h264parser.CodecData).PPS(), pkt.Data...)
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
				pkt.Data = append(codecs[0].(h264parser.CodecData).SPS(), pkt.Data...)
				pkt.Data = append(annexbNALUStartCode(), pkt.Data...)
			}

			bufferDuration := pkt.Time - previousTime
			previousTime = pkt.Time
			if err = outboundVideoTrack.WriteSample(media.Sample{Data: pkt.Data, Duration: bufferDuration}); err != nil && err != io.ErrClosedPipe {
				panic(err)
			}
		}

		if err = session.Close(); err != nil {
			log.Println("session Close error", err)
		}

		time.Sleep(5 * time.Second)
	}
}

func rtspConsumer2() {
	c := gortsplib.Client{}

	u, err := base.ParseURL(rtspURL)
	if err != nil {
		panic(err)
	}

	err = c.Start(u.Scheme, u.Host)
	if err != nil {
		panic(err)
	}
	defer c.Close()

	desc, _, err := c.Describe(u)
	if err != nil {
		panic(err)
	}

	var forma *format.H264
	medi := desc.FindFormat(&forma)
	if medi == nil {
		panic("media not found")
	}

	rtpDec, err := forma.CreateDecoder()
	if err != nil {
		panic(err)
	}

	_, err = c.Setup(desc.BaseURL, medi, 0, 0)
	if err != nil {
		panic(err)
	}

	var bufferDuration time.Duration
	var previousTimestamp uint32
	var clockRate = 90000

	c.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
		// pts, ok := c.PacketPTS2(medi, pkt)
		// if !ok {
		// 	log.Printf("Waiting for timestamp")
		// 	return
		// }

		// Calculate duration using the difference between successive RTP timestamps
		// Decode timestamp
		// pts, ok := c.PacketPTS2(medi, pkt)
		// if !ok {
		// 	log.Printf("Waiting for timestamp")
		// 	return
		// }

		// Extract access units from RTP packets
		nalUnits, err := rtpDec.Decode(pkt)
		if err != nil {
			if err != rtph264.ErrNonStartingPacketAndNoPrevious && err != rtph264.ErrMorePacketsNeeded {
				log.Printf("Error decoding RTP packet: %v", err)
			}
			return
		}

		if previousTimestamp != 0 {
			delta := pkt.Timestamp - previousTimestamp
			bufferDuration = time.Duration(float64(delta) / float64(clockRate) * float64(time.Second))
		}
		previousTimestamp = pkt.Timestamp

		au := convertToAnnexB(nalUnits, forma.SPS, forma.PPS)
		if err = outboundVideoTrack.WriteSample(media.Sample{Data: au, Duration: bufferDuration}); err != nil && err != io.ErrClosedPipe {
			panic(err)
		}

	})

	// start playing
	_, err = c.Play(nil)
	if err != nil {
		panic(err)
	}

	// wait until a fatal error
	panic(c.Wait())
}
