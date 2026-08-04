// Package fga e' um cliente HTTP minimo do OpenFGA, sem dependencias externas.
// Cobre exatamente o que o benchmark usa: CreateStore, WriteAuthorizationModel,
// Write, Check, ListObjects.
package fga

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type Client struct {
	BaseURL string
	HTTP    *http.Client
	StoreID string
	ModelID string
}

// New devolve um cliente com pool de conexoes dimensionado para carga.
// MaxIdleConnsPerHost e' o parametro que mais importa: no default (2) o gerador
// mediria estabelecimento de conexao TCP, nao latencia de autorizacao.
func New(baseURL string, conns int) *Client {
	return &Client{
		BaseURL: baseURL,
		HTTP: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   10 * time.Second,
					KeepAlive: 60 * time.Second,
				}).DialContext,
				MaxIdleConns:          conns * 2,
				MaxIdleConnsPerHost:   conns,
				MaxConnsPerHost:       conns,
				IdleConnTimeout:       90 * time.Second,
				DisableCompression:    true,
				ForceAttemptHTTP2:     false,
				ResponseHeaderTimeout: 120 * time.Second,
			},
		},
	}
}

func (c *Client) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s -> %d: %s", path, resp.StatusCode, clip(raw, 400))
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func clip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// ── Store e modelo ──────────────────────────────────────────────────────────

func (c *Client) CreateStore(ctx context.Context, name string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	if err := c.post(ctx, "/stores", map[string]string{"name": name}, &out); err != nil {
		return "", err
	}
	c.StoreID = out.ID
	return out.ID, nil
}

func (c *Client) WriteAuthorizationModel(ctx context.Context, model json.RawMessage) (string, error) {
	var out struct {
		ID string `json:"authorization_model_id"`
	}
	var body any
	if err := json.Unmarshal(model, &body); err != nil {
		return "", fmt.Errorf("modelo invalido: %w", err)
	}
	if err := c.post(ctx, "/stores/"+c.StoreID+"/authorization-models", body, &out); err != nil {
		return "", err
	}
	c.ModelID = out.ID
	return out.ID, nil
}

// ── Consultas ───────────────────────────────────────────────────────────────

type TupleKey struct {
	User     string `json:"user"`
	Relation string `json:"relation"`
	Object   string `json:"object"`
}

type checkReq struct {
	TupleKey             TupleKey `json:"tuple_key"`
	AuthorizationModelID string   `json:"authorization_model_id,omitempty"`
	Consistency          string   `json:"consistency,omitempty"`
}

type CheckResult struct {
	Allowed bool `json:"allowed"`
}

func (c *Client) Check(ctx context.Context, user, relation, object, consistency string) (bool, error) {
	var out CheckResult
	err := c.post(ctx, "/stores/"+c.StoreID+"/check", checkReq{
		TupleKey:             TupleKey{User: user, Relation: relation, Object: object},
		AuthorizationModelID: c.ModelID,
		Consistency:          consistency,
	}, &out)
	return out.Allowed, err
}

type listObjectsReq struct {
	Type                 string `json:"type"`
	Relation             string `json:"relation"`
	User                 string `json:"user"`
	AuthorizationModelID string `json:"authorization_model_id,omitempty"`
	Consistency          string `json:"consistency,omitempty"`
}

func (c *Client) ListObjects(ctx context.Context, typ, relation, user, consistency string) ([]string, error) {
	var out struct {
		Objects []string `json:"objects"`
	}
	err := c.post(ctx, "/stores/"+c.StoreID+"/list-objects", listObjectsReq{
		Type: typ, Relation: relation, User: user,
		AuthorizationModelID: c.ModelID, Consistency: consistency,
	}, &out)
	return out.Objects, err
}

// Write respeita o limite duro de 100 tuplas por chamada. Atencao: no validador
// do servidor, escritas e remocoes somam JUNTAS (60 + 50 = 110 = rejeitado).
func (c *Client) Write(ctx context.Context, writes, removals []TupleKey) error {
	if len(writes)+len(removals) > 100 {
		return fmt.Errorf("limite de 100 tuplas por Write excedido: %d + %d",
			len(writes), len(removals))
	}
	body := map[string]any{"authorization_model_id": c.ModelID}
	if len(writes) > 0 {
		body["writes"] = map[string]any{"tuple_keys": writes}
	}
	if len(removals) > 0 {
		body["deletes"] = map[string]any{"tuple_keys": removals}
	}
	return c.post(ctx, "/stores/"+c.StoreID+"/write", body, nil)
}

func (c *Client) Healthy(ctx context.Context) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/healthz", nil)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("healthz -> %d", resp.StatusCode)
	}
	return nil
}
