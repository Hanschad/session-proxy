package protocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{}

func TestAdapterHandshakeAndData(t *testing.T) {
	// 1. Setup Mock Server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Logf("Upgrade failed: %v", err)
			return
		}
		defer ws.Close()

		// A. Validate Init Handshake
		var initMsg map[string]string
		if err := ws.ReadJSON(&initMsg); err != nil {
			t.Errorf("Failed to read init msg: %v", err)
			return
		}

		if initMsg["TokenValue"] != "test-token" {
			t.Errorf("Expected token 'test-token', got %s", initMsg["TokenValue"])
		}

		// B. Simulate Echo (Read Data -> Write Msg back)
		for {
			mt, message, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.BinaryMessage {
				// Parse Input Frame
				agentMsg, err := UnmarshalMessage(message)
				if err != nil {
					t.Errorf("Server failed unmarshal: %v", err)
					continue
				}

				if agentMsg.Header.MessageType == MsgTypeInputStreamData {
					// Echo back as Output Stream Data
					resp, _ := NewInputMessage(agentMsg.Payload, 100)
					resp.Header.MessageType = MsgTypeOutputStreamData // Change type

					data, _ := resp.MarshalBinary()
					ws.WriteMessage(websocket.BinaryMessage, data)
				}
			}
		}
	}))
	defer server.Close()

	// 2. Connect Client
	DebugMode = true
	url := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	adapter, err := NewAdapter(ctx, url, "test-token")
	if err != nil {
		t.Fatalf("NewAdapter failed: %v", err)
	}
	defer adapter.Close()

	// 3. Test Write (Client -> Server)
	testPayload := []byte("Hello SSM")
	if _, err := adapter.Write(testPayload); err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// 4. Test Read (Server -> Client echo)
	buf := make([]byte, 1024)
	n, err := adapter.Read(buf)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}

	if string(buf[:n]) != string(testPayload) {
		t.Errorf("Expected '%s', got '%s'", testPayload, buf[:n])
	}
}
