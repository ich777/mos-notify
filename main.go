package main

import (
  "bytes"
  "encoding/json"
  "fmt"
  "io"
  "log"
  "net"
  "net/http"
  "net/url"
  "os"
  "path/filepath"
  "strconv"
  "strings"
  "text/template"
  "time"

  "github.com/gorilla/websocket"
)

// Structures
type Message struct {
  Title     string `json:"title"`
  Message   string `json:"message"`
  Priority  string `json:"priority"`
  Timestamp string `json:"timestamp"`
}

type ProviderConfig struct {
  Enabled      bool                   `json:"enabled"`
  User         interface{}            `json:"user"`
  Token        interface{}            `json:"token"`
  Url          string                 `json:"url"`
  Method       string                 `json:"method"`
  Headers      map[string]string      `json:"headers"`
  Body         map[string]interface{} `json:"body"`
  AlertMapping map[string]interface{} `json:"alert_mapping"`
}

type AppConfig struct {
  HttpPort int `json:"http_port"`
  WsPort   int `json:"ws_port"`
}

// Global vars
var (
  upgrader  = websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
  clients   = make(map[*websocket.Conn]bool)
  broadcast = make(chan Message, 100)
  providers = make(map[string]ProviderConfig)
  appcfg    = AppConfig{HttpPort: 999, WsPort: 999}
)

// Main
func main() {
  // Load ports.json
  if err := loadAppConfig("/boot/config/notify/ports.json"); err != nil {
    log.Printf("Couldn't load ports.json, using default ports: 999")
  }

  // Load providers
  cfgs, err := loadProviderConfigs("/boot/config/notify/providers")
  if err != nil {
    log.Fatalf("Error loading provider config: %v", err)
  }
  if len(cfgs) == 0 {
    log.Fatalf("No provider config in /boot/config/notify/providers found, please create one.")
  }
  providers = cfgs

  // Display overview - might be removed
  fmt.Println("Providers loaded:")
  for name, cfg := range providers {
    state := "enabled"
    if !cfg.Enabled {
      state = "disabled"
    }
    fmt.Printf("  - %s (%s)\n", name, state)
  }

  // Routes for receiving messages
  http.HandleFunc("/ws", handleConnections)
  http.HandleFunc("/send", handleSend)

  // Run dispatcher
  go handleMessages()

  // Run Unix Socket Server
  go startUnixSocketServer()

  addr := fmt.Sprintf(":%d", appcfg.HttpPort)
  fmt.Printf("Server start on port: %d\n", appcfg.HttpPort)
  fmt.Printf(" - WebSocket: ws://localhost:%d/ws\n", appcfg.WsPort)
  fmt.Printf(" - Send:    POST http://localhost:%d/send\n", appcfg.HttpPort)
  fmt.Printf(" - Socket:  echo 'Message' | /var/run/sock/mos-notify.sock\n")

  log.Fatal(http.ListenAndServe(addr, nil))
}

// Socket Server
func startUnixSocketServer() {
  socketPath := "/run/mos-notify.sock"
  
  // Create Socket path, shouldn't be necessary but just in case
  if err := os.MkdirAll("/run", 0755); err != nil {
    log.Printf("Konnte Socket-Verzeichnis nicht erstellen: %v", err)
    return
  }
  
  // Remove old Socket if exists
  os.Remove(socketPath)
  
  // Create Socket
  listener, err := net.Listen("unix", socketPath)
  if err != nil {
    log.Printf("Error creating Socket: %v", err)
    return
  }
  defer listener.Close()
  
  // chmod Socket for everyone
  os.Chmod(socketPath, 0666)
  
  log.Printf("Socket started: %s", socketPath)
  
  for {
    conn, err := listener.Accept()
    if err != nil {
      log.Printf("Socket Accept Error: %v", err)
      continue
    }
    
    go handleUnixSocketConnection(conn)
  }
}

// Socket handler
func handleUnixSocketConnection(conn net.Conn) {
  defer conn.Close()
  
  // Read message
  data := make([]byte, 1024)
  n, err := conn.Read(data)
  if err != nil {
    log.Printf("Socket read error: %v", err)
    return
  }
  
  messageText := strings.TrimSpace(string(data[:n]))
  if messageText == "" {
    return
  }

  // Just for debugging
  //log.Printf("Socket message: %s", messageText)
  
  // Try to parse JSON, if fail then handle as text
  var msg Message
  if err := json.Unmarshal([]byte(messageText), &msg); err != nil {
    msg = Message{
      Title:     "",
      Message:   messageText,
      Priority:  "normal",
      Timestamp: time.Now().Format(time.RFC3339),
    }
    log.Printf("Socket text message: %s", messageText)
  } else {
    log.Printf("Socket json message: title='%s', message='%s', priority='%s'", msg.Title, msg.Message, msg.Priority)
  }
  
  // Validate and send on broadcast or fail silently
  if validateAndFixMessage(&msg) {
    broadcast <- msg
    // Just for troubleshooting
    //conn.Write([]byte("ok\n"))
  }
}

// WebSocket
func handleConnections(w http.ResponseWriter, r *http.Request) {
  ws, err := upgrader.Upgrade(w, r, nil)
  if err != nil {
    log.Printf("Upgrade error: %v", err)
    return
  }
  defer ws.Close()
  clients[ws] = true
  log.Println("Client connected")
  for {
    if _, _, err := ws.ReadMessage(); err != nil {
      delete(clients, ws)
      log.Println("Client disconnected")
      break
    }
  }
}

// Message handler
func handleMessages() {
  for {
    msg := <-broadcast
    // Send to WebSocket
    for client := range clients {
      if err := client.WriteJSON(msg); err != nil {
        client.Close()
        delete(clients, client)
      }
    }
    // Send to provider(s) if enabled
    for name, cfg := range providers {
      if !cfg.Enabled {
        continue
      }
      go sendToProvider(name, cfg, msg)
    }
  }
}

// HTTP /send
func handleSend(w http.ResponseWriter, r *http.Request) {
  body, _ := io.ReadAll(r.Body)
  defer r.Body.Close()
  var msg Message
  // Try to parse JSON, if fail then handle as text
  if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
    if err := json.Unmarshal(body, &msg); err != nil {
      http.Error(w, "Invalid JSON", 400)
      return
    }
  } else {
    msg = Message{
      Title:    "",
      Message:  string(body),
      Priority: "normal",
    }
  }

  // Validate and send on broadcast or fail silently
  if validateAndFixMessage(&msg) {
    broadcast <- msg
    // Just for troubleshooting
    //conn.Write([]byte("ok\n"))
  }
}

// Message validation
func validateAndFixMessage(msg *Message) bool {
  if strings.TrimSpace(msg.Message) == "" {
    return false
  }
  if msg.Priority == "" {
    msg.Priority = "normal"
  }
  if msg.Timestamp == "" {
    msg.Timestamp = time.Now().Format(time.RFC3339)
  }
  if strings.TrimSpace(msg.Title) == "" {
    if hn, err := os.Hostname(); err == nil {
      msg.Title = hn
    } else {
      msg.Title = "HostnameError"
    }
  }
  return true
}

// Config loader
func loadAppConfig(path string) error {
  data, err := os.ReadFile(path)
  if err != nil {
    return err
  }
  return json.Unmarshal(data, &appcfg)
}

// Config loader for providers
func loadProviderConfigs(dir string) (map[string]ProviderConfig, error) {
  configs := make(map[string]ProviderConfig)
  files, err := os.ReadDir(dir)
  if err != nil {
    return nil, err
  }
  for _, f := range files {
    if !f.IsDir() && strings.HasSuffix(f.Name(), ".json") {
      data, err := os.ReadFile(filepath.Join(dir, f.Name()))
      if err != nil {
        return nil, fmt.Errorf("Config-Fehler %s: %v", f.Name(), err)
      }
      var cfg ProviderConfig
      if err := json.Unmarshal(data, &cfg); err != nil {
        return nil, fmt.Errorf("Config-Fehler %s: %v", f.Name(), err)
      }
      configs[strings.TrimSuffix(f.Name(), ".json")] = cfg
    }
  }
  return configs, nil
}

// Provider send function
func sendToProvider(name string, cfg ProviderConfig, msg Message) {
  mapped := msg.Priority
  if val, ok := cfg.AlertMapping[msg.Priority]; ok {
    mapped = fmt.Sprintf("%v", val)
  }
  data := buildTemplateData(msg, cfg, mapped)
  // Render body
  result := make(map[string]interface{})
  for k, v := range cfg.Body {
    switch val := v.(type) {
    case string:
      result[k] = renderTemplate(val, data)
    case map[string]interface{}:
      if numTempl, ok := val["$number"].(string); ok {
        rendered := renderTemplate(numTempl, data)
        if num, err := strconv.Atoi(rendered); err == nil {
          result[k] = num
        } else {
          result[k] = rendered
        }
      } else {
        result[k] = val
      }
    default:
      result[k] = val
    }
  }
  var reqBody io.Reader
  if ct, ok := cfg.Headers["Content-Type"]; ok && strings.Contains(ct, "json") {
    b, _ := json.Marshal(result)
    reqBody = bytes.NewBuffer(b)
  } else {
    form := url.Values{}
    for k, v := range result {
      form.Set(k, fmt.Sprintf("%v", v))
    }
    reqBody = strings.NewReader(form.Encode())
  }
  finalURL := renderTemplate(cfg.Url, data)
  req, _ := http.NewRequest(cfg.Method, finalURL, reqBody)
  for k, v := range cfg.Headers {
    req.Header.Set(k, v)
  }
  resp, err := http.DefaultClient.Do(req)
  if err != nil {
    // Just for debugging
    //log.Printf("✗ %s Error: %v", name, err)
    return
  }
  defer resp.Body.Close()
  // Just for debugging
  //log.Printf("✓ Provider %s → %s", name, resp.Status)
}

// Helper
func buildTemplateData(msg Message, cfg ProviderConfig, mappedPriority string) map[string]string {
  data := map[string]string{
    "Title":    msg.Title,
    "Message":  msg.Message,
    "Priority": mappedPriority,
    "Time":     msg.Timestamp,
  }
  if u, ok := cfg.User.(string); ok && u != "" {
    data["User"] = u
  }
  if t, ok := cfg.Token.(string); ok && t != "" {
    data["Token"] = t
  }
  return data
}

func renderTemplate(tmpl string, data map[string]string) string {
  t, err := template.New("tmpl").Parse(tmpl)
  if err != nil {
    return tmpl
  }
  var buf bytes.Buffer
  if err := t.Execute(&buf, data); err != nil {
    return tmpl
  }
  return buf.String()
}

// go mod tidy && go build -ldflags="-s -w"
