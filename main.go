package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

type ConnMutex struct {
	mu   sync.Mutex
	conn *websocket.Conn
}

// Map holding all Websocket clients and the endpoints they are subscribed to
var clients = make(map[string][]*ConnMutex)
var clientsMu = sync.Mutex{}
var upgrader = websocket.Upgrader{}

// Message which will be sent as JSON to Websocket clients
type Message struct {
	Headers  map[string]string `json:"headers"`
	Endpoint string            `json:"endpoint"`
	Data     interface{}       `json:"data"`
}

func removeConn(conn *ConnMutex, endpoint string, reason string) {
	logEntry := log.WithField("endpoint", endpoint).WithField("reason", reason)

	conns := clients[endpoint]
	clientsMu.Lock()
	for i, cm := range conns {
		if cm == conn {
			// Remove element from array
			conns[i] = conns[len(conns)-1]
			conns = conns[:len(conns)-1]
			// Close connection
			conn.conn.Close()

			clients[endpoint] = conns
			logEntry.WithField("clients", len(conns)).Infoln("Client disconnected")
			break
		}
	}
	clientsMu.Unlock()
}

func keepAlive(cm *ConnMutex, endpoint string, timeout time.Duration) {
	lastResponse := time.Now()

	cm.conn.SetCloseHandler(func(code int, text string) error {
		removeConn(cm, endpoint, "disconnect")
		return nil
	})

	go func() {
		for {
			cm.mu.Lock()
			err := cm.conn.WriteMessage(websocket.TextMessage, []byte("heartbeat"))
			cm.mu.Unlock()
			if err != nil {
				removeConn(cm, endpoint, "write message error: "+err.Error())
				return
			}

			cm.conn.SetReadDeadline(time.Now().Add(timeout))
			messageType, message, err := cm.conn.ReadMessage()
			if err != nil {
				removeConn(cm, endpoint, "read message timeout: "+err.Error())
				return
			}

			if messageType == websocket.TextMessage && string(message) == "heartbeat" {
				lastResponse = time.Now()
			}

			if time.Since(lastResponse) > timeout {
				removeConn(cm, endpoint, "heartbeat timeout")
				return
			}

			time.Sleep(timeout)
		}
	}()
}

func handleHook(w http.ResponseWriter, r *http.Request, endpoint string) {
	msg := Message{}
	logEntry := log.WithField("endpoint", endpoint)

	// Transfer headers to response
	msg.Headers = make(map[string]string)
	for k, v := range r.Header {
		msg.Headers[k] = v[0]
	}

	// Set endpoint on response
	msg.Endpoint = endpoint

	// Read body of request
	buf := new(bytes.Buffer)
	buf.ReadFrom(r.Body)

	// If request is JSON, unmarshal and save to response. Otherwise just save as string.
	if r.Header.Get("Content-Type") == "application/json" {
		json.Unmarshal(buf.Bytes(), &msg.Data)
	} else {
		msg.Data = buf.Bytes()
	}

	// Get all clients listening to the current endpoint
	conns := clients[endpoint]

	if conns != nil {
		if len(conns) != 0 {
			clientsMu.Lock()
			for _, cm := range conns {
				cm.mu.Lock()
				cm.conn.WriteJSON(msg)
				cm.mu.Unlock()
			}
			clientsMu.Unlock()

			// Send 202-Accepted status code if sent to any client
			w.WriteHeader(202)
		}
	}

	logEntry.WithField("clients", len(conns)).Infoln("Hook broadcasted")
}

func handleClient(w http.ResponseWriter, r *http.Request, endpoint string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	cm := ConnMutex{conn: conn}

	logEntry := log.WithField("endpoint", endpoint)

	if err != nil {
		logEntry.Println(err)
		// Send Upgrade required response if upgrade fails
		w.WriteHeader(426)
		return
	}

	// Ensure connection is kept alive
	keepAlive(&cm, endpoint, time.Second*30)
	// Add client to endpoint slice
	clientsMu.Lock()
	clients[endpoint] = append(clients[endpoint], &cm)
	logEntry.WithField("clients", len(clients[endpoint])).Infoln("Client connected")
	clientsMu.Unlock()
}

func handler(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimRight(r.URL.Path, "/")

	/**
	 * Check prefix of URL path:
	 * 	/hook is used for webhooks and requests will be broadcasted to all listening clients.
	 * 	/socket is used for connect a new socket client
	 */
	if strings.HasPrefix(path, "/hook") {
		handleHook(w, r, strings.TrimPrefix(path, "/hook"))
	} else if strings.HasPrefix(path, "/socket") {
		handleClient(w, r, strings.TrimPrefix(path, "/socket"))
	} else {
		log.WithField("path", r.URL.Path).Warnln("404 Not found")
		w.WriteHeader(404)
	}
}

func main() {
	// Get command line options --address and --port
	address := flag.String("address", "", "Address to bind to.")
	port := flag.Int("port", 1234, "Port to bind to. Default: 1234")
	flag.Parse()
	upgrader.CheckOrigin = func(r *http.Request) bool { return true }

	http.HandleFunc("/", handler)

	// Start HTTP server
	log.Infof("Sockethook is ready and listening at port %d ✅", *port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf("%s:%d", *address, *port), nil))
}
