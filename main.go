package main

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fatih/color"
	"github.com/joho/godotenv"
	"github.com/valyala/fasthttp"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type SSL struct {
	PublicKey  string `bson:"public_key"`
	PrivateKey string `bson:"private_key"`
}

func GetEnv(key string, default_value string) string {
	value := os.Getenv(key)
	if value == "" {
		value = default_value
	}
	return value
}

type HostMapping struct {
	HostURL   string         `bson:"host_url"`
	TargetURL []string       `bson:"target_url"`
	Timeout   *time.Duration `bson:"timeout,omitempty"`
	MaxConns  *int           `bson:"max_conns,omitempty"`
	SSL       *SSL           `bson:"ssl"`
}

func loadHostMapping(host string) (*HostMapping, error) {
	clientOptions := options.Client().ApplyURI(GetEnv("MONGO_URL", "mongodb://localhost:27017"))
	client, err := mongo.Connect(context.Background(), clientOptions)
	if err != nil {
		return nil, err
	}
	defer client.Disconnect(context.Background())
	database := client.Database(GetEnv("MONGO_NAME", "api_gateway"))

	hostsCollection := database.Collection("hosts")
	var mapping HostMapping
	err = hostsCollection.FindOne(context.Background(), bson.M{"host_url": host}).Decode(&mapping)
	if err != nil {
		return nil, err
	}

	return &mapping, nil
}

func colorizeStatusCode(statusCode int) string {
	switch {
	case statusCode >= 200 && statusCode < 300:
		return color.New(color.FgGreen).Sprint(statusCode)
	case statusCode >= 300 && statusCode < 400:
		return color.New(color.FgBlue).Sprint(statusCode)
	case statusCode >= 400 && statusCode < 500:
		return color.New(color.FgYellow).Sprint(statusCode)
	case statusCode >= 500:
		return color.New(color.FgRed).Sprint(statusCode)
	default:
		return color.New(color.FgWhite).Sprint(statusCode)
	}
}

type Server struct {
	ActiveConns    map[string]map[string]int
	ActiveConnsMux sync.Mutex
}

func (s *Server) getNextTargetURL(host string) string {
	s.ActiveConnsMux.Lock()
	defer s.ActiveConnsMux.Unlock()

	mapping, err := loadHostMapping(host)
	if err != nil {
		log.Printf("Error loading host mapping: %s", err)
		return ""
	}

	// Check if ActiveConns map is initialized
	if s.ActiveConns == nil {
		log.Println("ActiveConns map is nil")
		s.ActiveConns = make(map[string]map[string]int)
	}

	targetURLs := mapping.TargetURL

	minConns := int(^uint(0) >> 1) // Max int value
	var targetURL string

	for _, url := range targetURLs {
		if s.ActiveConns[host] == nil {
			s.ActiveConns[host] = make(map[string]int)
		}
		conns := s.ActiveConns[host][url]
		if conns < minConns {
			minConns = conns
			targetURL = url
		}
	}

	// Increase the active connections count for the selected target
	s.ActiveConns[host][targetURL]++

	return targetURL
}

func (s *Server) releaseConnection(host, targetURL string) {
	s.ActiveConnsMux.Lock()
	defer s.ActiveConnsMux.Unlock()
	s.ActiveConns[host][targetURL]--
}

func (s *Server) reverseProxyHandler(ctx *fasthttp.RequestCtx) {
	host := string(ctx.Host())

	mapping, err := loadHostMapping(host)
	if err != nil {
		ctx.Error("Service Unavailable", fasthttp.StatusServiceUnavailable)
		log.Printf("Error loading host mapping: %s", err)
		return
	}

	targetURL := s.getNextTargetURL(host)
	if targetURL == "" {
		ctx.Error("Service Unavailable", fasthttp.StatusServiceUnavailable)
		return
	}

	if !strings.HasPrefix(targetURL, "http://") && !strings.HasPrefix(targetURL, "https://") {
		if strings.Contains(targetURL, "localhost") {
			targetURL = "http://" + targetURL
		} else {
			targetURL = "https://" + targetURL
		}
	}
	target, err := url.Parse(targetURL)
	if err != nil {
		ctx.Error("Invalid target URL", fasthttp.StatusInternalServerError)
		s.releaseConnection(host, targetURL)
		log.Printf("Error parsing target URL: %s", err)
		return
	}

	defer s.releaseConnection(host, targetURL)

	// Check if the request is an upgrade to WebSocket by inspecting the Upgrade header
	if strings.ToLower(string(ctx.Request.Header.Peek("Upgrade"))) == "websocket" {
		// Respond with HTTP 101 status (Switching Protocols) to indicate WebSocket upgrade
		ctx.Response.Header.Set("Upgrade", "websocket")
		ctx.Response.Header.Set("Connection", "Upgrade")
		ctx.SetStatusCode(fasthttp.StatusSwitchingProtocols)
		return
	}

	req := &ctx.Request
	req.SetRequestURI(target.ResolveReference(&url.URL{Path: string(ctx.Path())}).String())
	req.Header.SetHost(target.Host)

	// Copy headers
	ctx.Request.Header.VisitAll(func(key, value []byte) {
		req.Header.SetBytesKV(key, value)
	})

	// Handle non-WebSocket HTTP requests using the reverse proxy
	proxyClient := &fasthttp.HostClient{
		Addr: target.Host,
	}

	if mapping.MaxConns != nil {
		proxyClient.MaxConns = *mapping.MaxConns
	}
	if mapping.Timeout != nil {
		proxyClient.ReadTimeout = *mapping.Timeout
		proxyClient.WriteTimeout = *mapping.Timeout
	}

	// Perform the reverse proxy request
	start := time.Now()
	if err := proxyClient.Do(req, &ctx.Response); err != nil {
		log.Printf("Error during request: %s", err)
		ctx.Error("Error during request", fasthttp.StatusInternalServerError)
		s.releaseConnection(host, targetURL)
	}

	duration := time.Since(start)
	statusCode := ctx.Response.StatusCode()
	colorStatusCode := colorizeStatusCode(statusCode)
	log.Printf("| %s | %s | %s | %s | %s | %s",
		colorStatusCode,
		duration,
		ctx.RemoteIP(),
		string(ctx.Method()),
		targetURL,
		string(ctx.Path()))
}

func main() {
	pwd, _ := os.Getwd()
	envFilePath := filepath.Join(pwd, ".env")
	err := godotenv.Load(envFilePath)
	if err != nil {
		fmt.Println("file .env tidak ditemukan")
	}

	server := &Server{
		ActiveConns: make(map[string]map[string]int),
	}

	requestHandler := func(ctx *fasthttp.RequestCtx) {
		server.reverseProxyHandler(ctx)
	}

	if err := fasthttp.ListenAndServe("127.0.0.1:8880", requestHandler); err != nil {
		log.Fatalf("Error in ListenAndServe: %s", err)
	}
}
