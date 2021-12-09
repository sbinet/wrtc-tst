package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

//go:embed index.html
var rootHTML string

//go:embed script.js
var rootJS string

var ctlAddr string

func main() {
	const (
		addr = ":8000"
		cert = "server.crt"
		key  = "server.key"
	)

	flag.StringVar(&ctlAddr, "ctl-addr", "rpi-1.local:5000", "")
	flag.Parse()

	http.HandleFunc("/", rootHandle)
	http.HandleFunc("/ws", wsHandle)
	http.HandleFunc("/js/main.js", jsHandle)

	err := http.ListenAndServeTLS(addr, cert, key, nil)
	if err != nil {
		log.Fatal(err)
	}
}

func rootHandle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, rootHTML)
}

func jsHandle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	fmt.Fprint(w, rootJS)
}

var (
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
	}
)

func wsHandle(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			log.Printf("could not upgrade websocket connection: %+v", err)
		}
		return
	}

	conn, err := net.Dial("tcp", ctlAddr)
	if err != nil {
		log.Printf("could not dial clrmelies-srv: %+v", err)
		return
	}
	defer conn.Close()

	cmd, err := recv(conn)
	if err != nil {
		log.Printf("could not recv 'ready' cmd: %+v", err)
		return
	}
	if cmd.Name != "ready" {
		log.Printf("recv invalid cmd: %+v", cmd)
		return
	}

	for {
		_, buf, err := ws.ReadMessage()
		if err != nil {
			log.Printf("could not read websocket message: %+v", err)
			break
		}

		err = json.Unmarshal(buf, &cmd)
		if err != nil {
			log.Printf("could not unmarshal websocket message: %+v", err)
			return
		}

		err = handleMsg(conn, ws, cmd)
		if err != nil {
			log.Printf("could not handle websocket message: %+v", err)
			continue
		}
	}
}

func handleMsg(conn net.Conn, ws *websocket.Conn, msg Cmd) error {
	log.Printf("--- msg: %+v", msg.Name)
	switch msg.Name {
	case "start":
		err := send(conn, Cmd{Name: "start"})
		if err != nil {
			return fmt.Errorf("could not send start cmd: %w", err)
		}
		rep, err := recv(conn)
		if err != nil {
			return fmt.Errorf("could not recv reply: %w", err)
		}
		if rep.Name != "ok" {
			return fmt.Errorf("could not recv reply: %+v", rep)
		}

	case "stop":
		err := send(conn, Cmd{Name: "stop"})
		if err != nil {
			return fmt.Errorf("could not send start cmd: %w", err)
		}
		rep, err := recv(conn)
		if err != nil {
			return fmt.Errorf("could not recv reply: %w", err)
		}
		if rep.Name != "ok" {
			return fmt.Errorf("could not recv reply: %+v", rep)
		}

	case "offer":
		var offer webrtc.SessionDescription
		if err := json.Unmarshal([]byte(msg.Data), &offer); err != nil {
			return err
		}

		err := send(conn, Cmd{Name: "offer", Data: encode64(offer)})
		if err != nil {
			return fmt.Errorf("could not send offer: %w", err)
		}

		rep, err := recv(conn)
		if err != nil {
			return fmt.Errorf("could not recv reply: %w", err)
		}
		if rep.Name != "answer" {
			return fmt.Errorf("could not recv reply: %+v", rep)
		}

		log.Printf("recv answer")

		err = ws.WriteJSON(rep)
		if err != nil {
			return fmt.Errorf("could not fwd reply to ws: %w", err)
		}

	default:
		log.Printf("**error** unknown event %q", msg.Name)
	}
	return nil
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

func encode64(obj interface{}) string {
	b, err := json.Marshal(obj)
	if err != nil {
		panic(err)
	}

	return base64.StdEncoding.EncodeToString(b)
}
