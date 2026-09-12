package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

type config struct {
	GatewayURL string
	Email      string
	Password   string
	TenantID   string
	ListenAddr string
}

type server struct {
	client *http.Client
	cfg    config
}

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type loginResponse struct {
	AccessToken string `json:"access_token"`
}

type contact struct {
	ID       string `json:"id"`
	TenantID string `json:"tenant_id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
}

type contactsResponse struct {
	Contacts []contact `json:"contacts"`
}

func main() {
	cfg := loadConfig()
	srv := &server{
		client: &http.Client{Timeout: 10 * time.Second},
		cfg:    cfg,
	}

	app := fiber.New()
	app.Get("/healthz", srv.handleHealth)
	app.Post("/sync", srv.handleSync)
	app.Get("/sync", srv.handleSync)

	log.Printf("contactsync plugin listening on %s", cfg.ListenAddr)
	if err := app.Listen(cfg.ListenAddr); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() config {
	return config{
		GatewayURL: envOr("SANNAD_GATEWAY_URL", "http://localhost:8080"),
		Email:      envOr("SANNAD_PLUGIN_EMAIL", "admin@example.com"),
		Password:   envOr("SANNAD_PLUGIN_PASSWORD", "StrongPass1234"),
		TenantID:   envOr("SANNAD_PLUGIN_TENANT", "demo"),
		ListenAddr: envOr("SANNAD_PLUGIN_ADDR", ":8090"),
	}
}

func envOr(key string, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}

	return fallback
}

func (srv *server) handleHealth(ctx *fiber.Ctx) error {
	return ctx.Status(fiber.StatusOK).JSON(map[string]string{
		"service": "contactsync-plugin",
		"target":  srv.cfg.GatewayURL,
		"tenant":  srv.cfg.TenantID,
	})
}

func (srv *server) handleSync(ctx *fiber.Ctx) error {
	accessToken, err := srv.login()
	if err != nil {
		return writeError(ctx, fiber.StatusBadGateway, err)
	}

	contacts, err := srv.fetchContacts(accessToken)
	if err != nil {
		return writeError(ctx, fiber.StatusBadGateway, err)
	}

	return ctx.Status(fiber.StatusOK).JSON(map[string]any{
		"plugin":    "contactsync",
		"tenant_id": srv.cfg.TenantID,
		"contacts":  contacts,
		"count":     len(contacts),
	})
}

func (srv *server) login() (string, error) {
	payloadBytes, err := json.Marshal(loginRequest{
		Email:    srv.cfg.Email,
		Password: srv.cfg.Password,
	})
	if err != nil {
		return "", err
	}

	response, err := srv.client.Post(srv.cfg.GatewayURL+"/api/v1/auth/login", "application/json", bytes.NewReader(payloadBytes))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", decodeError(response)
	}

	var body loginResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return "", err
	}

	if body.AccessToken == "" {
		return "", errors.New("plugin: gateway login returned empty access token")
	}

	return body.AccessToken, nil
}

func (srv *server) fetchContacts(accessToken string) ([]contact, error) {
	contactsURL, err := url.Parse(srv.cfg.GatewayURL + "/api/v1/crm/contacts")
	if err != nil {
		return nil, err
	}

	query := contactsURL.Query()
	query.Set("tenant", srv.cfg.TenantID)
	contactsURL.RawQuery = query.Encode()

	request, err := http.NewRequest(http.MethodGet, contactsURL.String(), nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)

	response, err := srv.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return nil, decodeError(response)
	}

	var body contactsResponse
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		return nil, err
	}

	return body.Contacts, nil
}

func decodeError(response *http.Response) error {
	var body map[string]string
	if err := json.NewDecoder(response.Body).Decode(&body); err == nil {
		if body["error"] != "" {
			return errors.New(body["error"])
		}
	}

	return errors.New(strings.TrimSpace(response.Status))
}

func writeError(ctx *fiber.Ctx, statusCode int, err error) error {
	return ctx.Status(statusCode).JSON(map[string]string{"error": err.Error()})
}
