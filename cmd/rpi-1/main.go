package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
)

func main() {
	var (
		addr = flag.String("addr", ":5000", "address to listen for commands")
	)

	flag.Parse()

	l, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("could not listen on %q: %+v", *addr, err)
	}
	defer l.Close()

	tmp, err := os.MkdirTemp("", "tmp-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)

	sdp := filepath.Join(tmp, "stream.sdp")
	err = os.WriteFile(sdp, []byte(streamSDP), 0644)
	if err != nil {
		panic(err)
	}

	for {
		log.Printf(">>> accepting...")
		conn, err := l.Accept()
		if err != nil {
			log.Printf("could not accept connection: %+v", err)
			continue
		}
		go serve(conn, sdp)
	}
}

type udpConn struct {
	conn        *net.UDPConn
	port        int
	payloadType uint8
}

func serve(conn net.Conn, sdp string) {
	log.Printf(">>> serving... %+v", conn.RemoteAddr())
	defer conn.Close()

	var (
		engine webrtc.MediaEngine
		err    error
	)

	err = engine.RegisterCodec(
		webrtc.RTPCodecParameters{
			RTPCodecCapability: webrtc.RTPCodecCapability{
				MimeType:     webrtc.MimeTypeVP8,
				ClockRate:    90000,
				Channels:     0,
				SDPFmtpLine:  "",
				RTCPFeedback: nil,
			},
		},
		webrtc.RTPCodecTypeVideo,
	)
	if err != nil {
		panic(err)
	}

	// Create an InterceptorRegistry. This is the user configurable RTP/RTCP Pipeline.
	// This provides NACKs, RTCP Reports and other features. If you use `webrtc.NewPeerConnection`
	// this is enabled by default. If you are manually managing You MUST create a InterceptorRegistry
	// for each PeerConnection.
	var ireg interceptor.Registry

	// Use the default set of Interceptors
	err = webrtc.RegisterDefaultInterceptors(&engine, &ireg)
	if err != nil {
		panic(err)
	}

	// Create the API object with the MediaEngine
	api := webrtc.NewAPI(webrtc.WithMediaEngine(&engine), webrtc.WithInterceptorRegistry(&ireg))

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	pc, err := api.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}
	defer func() {
		err := pc.Close()
		if err != nil {
			fmt.Printf("cannot close peerConnection: %v\n", err)
		}
	}()

	// Allow us to receive 1 video track
	_, err = pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	if err != nil {
		panic(err)
	}

	// Create a local addr
	laddr, err := net.ResolveUDPAddr("udp", "127.0.0.1:")
	if err != nil {
		panic(err)
	}

	// Prepare udp conns
	// Also update incoming packets with expected PayloadType, the browser may use
	// a different value. We have to modify so our stream matches what rtp-forwarder.sdp expects
	udpConns := map[string]*udpConn{
		//"audio": {port: 4000, payloadType: 111},
		"video": {port: 4002, payloadType: 96},
	}
	for _, c := range udpConns {
		// Create remote addr
		raddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("127.0.0.1:%d", c.port))
		if err != nil {
			panic(err)
		}

		// Dial udp
		c.conn, err = net.DialUDP("udp", laddr, raddr)
		if err != nil {
			panic(err)
		}
		defer func(conn net.PacketConn) {
			err := conn.Close()
			if err != nil {
				panic(err)
			}
		}(c.conn)
	}

	// Set a handler for when a new remote track starts, this handler will forward data to
	// our UDP listeners.
	// In your application this is where you would handle/process audio/video
	pc.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		// Retrieve udp connection
		c, ok := udpConns[track.Kind().String()]
		if !ok {
			return
		}

		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		go func() {
			ticker := time.NewTicker(time.Second * 2)
			defer ticker.Stop()
			loss := new(rtcp.PictureLossIndication)
			pkts := []rtcp.Packet{loss}
			for range ticker.C {
				loss.MediaSSRC = uint32(track.SSRC())
				err := pc.WriteRTCP(pkts)
				if err != nil {
					fmt.Println(err)
				}
			}
		}()

		b := make([]byte, 1500)
		rtpPacket := &rtp.Packet{}
		for {
			// Read
			n, _, readErr := track.Read(b)
			if readErr != nil {
				panic(readErr)
			}

			// Unmarshal the packet and update the PayloadType
			if err = rtpPacket.Unmarshal(b[:n]); err != nil {
				panic(err)
			}
			rtpPacket.PayloadType = c.payloadType

			// Marshal into original buffer with updated PayloadType
			if n, err = rtpPacket.MarshalTo(b); err != nil {
				panic(err)
			}

			// Write
			if _, err = c.conn.Write(b[:n]); err != nil {
				// For this particular example, third party applications usually timeout after a short
				// amount of time during which the user doesn't have enough time to provide the answer
				// to the browser.
				// That's why, for this particular example, the user first needs to provide the answer
				// to the browser then open the third party application. Therefore we must not kill
				// the forward on "connection refused" errors
				if opError, ok := err.(*net.OpError); ok && opError.Err.Error() == "write: connection refused" {
					continue
				}
				panic(err)
			}
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	pc.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("Connection State has changed %s \n", connectionState.String())

		if connectionState == webrtc.ICEConnectionStateConnected {
			fmt.Println("Ctrl+C the remote client to stop the demo")
		}
	})

	// Set the handler for Peer connection state
	// This will notify you when the peer has connected/disconnected
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())

		if s == webrtc.PeerConnectionStateFailed {
			// Wait until PeerConnection has had no network activity for 30 seconds or another failure. It may be reconnected using an ICE Restart.
			// Use webrtc.PeerConnectionStateDisconnected if you are interested in detecting faster timeout.
			// Note that the PeerConnection may come back from PeerConnectionStateDisconnected.
			fmt.Println("Done forwarding")
			os.Exit(0)
		}
	})

	err = send(conn, Cmd{Name: "ready"})
	if err != nil {
		panic(err)
	}

	cmd, err := recv(conn)
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}
	log.Printf("recv-1: %q", cmd.Name)

	// Wait for the offer to be pasted
	offer := webrtc.SessionDescription{}
	err = decode64(cmd.Data, &offer)
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	// Set the remote SessionDescription
	if err = pc.SetRemoteDescription(offer); err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	// Create answer
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	// Create channel that is blocked until ICE Gathering is complete
	gatherComplete := webrtc.GatheringCompletePromise(pc)

	// Sets the LocalDescription, and starts our UDP listeners
	if err = pc.SetLocalDescription(answer); err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	// Block until ICE Gathering is complete, disabling trickle ICE
	// we do this because we only can exchange one signaling message
	// in a production application you should exchange ICE Candidates via OnICECandidate
	<-gatherComplete

	log.Printf("gather complete")

	raw, err := json.Marshal(pc.LocalDescription())
	if err != nil {
		panic(err)
	}

	err = send(conn, Cmd{Name: "answer", Data: string(raw)})
	if err != nil {
		panic(err)
	}

	// Output the answer in base64 so we can paste it in browser
	//fmt.Println(signal.Encode(*peerConnection.LocalDescription()))

	log.Printf("waiting for start cmd...")
	// wait for start command
	cmd, err = recv(conn)
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	if cmd.Name != "start" {
		_ = send(conn, Cmd{Name: "error", Data: "unexpected command"})
		panic(fmt.Errorf("unexpected command %+v", cmd))
	}

	//video := exec.Command("vlc", "--fullscreen", sdp)
	video := exec.Command("vlc", sdp)
	//video := exec.Command("ffplay", "-i", sdp, "-protocol_whitelist", "file,udp,rtp")
	video.Stdout = log.Writer()
	video.Stderr = log.Writer()
	err = video.Start()
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}
	defer video.Process.Kill()

	err = send(conn, Cmd{Name: "ok"})
	if err != nil {
		panic(err)
	}

	log.Printf("waiting for stop cmd...")
	// wait for stop command
	cmd, err = recv(conn)
	if err != nil {
		_ = send(conn, Cmd{Name: "error", Data: err.Error()})
		panic(err)
	}

	if cmd.Name != "stop" {
		panic(fmt.Errorf("unexpected command %+v", cmd))
	}

	err = send(conn, Cmd{Name: "ok"})
	if err != nil {
		panic(err)
	}
}

type Cmd struct {
	Name string `json:"name"`
	Data string `json:"data"`
}

func send(w io.Writer, cmd Cmd) error {
	buf, err := json.Marshal(cmd)
	if err != nil {
		return fmt.Errorf("could not marshal command: %w", err)
	}

	n, err := w.Write(buf)
	if err != nil {
		return fmt.Errorf("could not send command: %w", err)
	}
	if n != len(buf) {
		return fmt.Errorf("could not send command: short write (got=%d, want=%d)", n, len(buf))
	}

	return nil
}

func recv(r io.Reader) (Cmd, error) {
	var cmd Cmd
	err := json.NewDecoder(r).Decode(&cmd)
	if err != nil {
		return cmd, fmt.Errorf("could not decode command: %w", err)
	}

	return cmd, nil
}

func decode64(v string, ptr interface{}) error {
	raw, err := base64.StdEncoding.DecodeString(v)
	if err != nil {
		return err
	}

	return json.Unmarshal(raw, ptr)
}

func encode64(obj interface{}) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}

const streamSDP = `v=0
o=- 0 0 IN IP4 127.0.0.1
s=Melies stream
c=IN IP4 127.0.0.1
t=0 0
a=recvonly
m=video 4002 RTP/AVP 96
a=rtpmap:96 VP8/90000
`
