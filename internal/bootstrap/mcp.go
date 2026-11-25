package bootstrap

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"go.idp.xyz/idp-cargo/internal/contexts/fin"
	finapi "go.idp.xyz/idp-cargo/internal/contexts/fin/api"
	mcptransport "go.idp.xyz/idp-cargo/internal/transport/mcp"
)

// MCPConfig configures the MCP server behind the IDP TELOS gateway (V2.101). It is read from a JSON file: the endpoint
// as it is registered in idp-mcp (the audience of every assertion, ending in /mcp), the gateway's id, the files of the
// gateway's public key (PEM, P-256; several while it is rotated) and of the cursors' HMAC key (base64, 32 bytes or
// more), what CargoPai lets each gateway tenant and role read, and the limits. There is no switch that turns the
// verification of the caller off, and the server does not start without a valid public key.
type MCPConfig struct {
	Audience      string `json:"audience"`
	GatewayID     string `json:"gateway_id"`
	PublicKeyFile string `json:"public_key_file"`
	CursorKeyFile string `json:"cursor_key_file"`
	fin.MCPPolicy
	Limits struct {
		CallsPerMinute int `json:"calls_per_minute"`
		Concurrent     int `json:"concurrent"`
	} `json:"limits"`

	// PublicKeys and CursorKey are what the two files hold: LoadMCPConfig reads them.
	PublicKeys []byte `json:"-"`
	CursorKey  []byte `json:"-"`

	authorizer finapi.Authorizer // a test's, in place of the permissions of the principal
}

var gatewayID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// LoadMCPConfig reads the configuration file and the two key files it names (a relative path is relative to the
// configuration file) and refuses what is not valid: an unknown key, an audience that does not end in /mcp, a policy
// that grants more than FIN's read permission, a public key that is not P-256, a cursor key under 32 bytes.
func LoadMCPConfig(path string) (MCPConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return MCPConfig{}, err
	}
	var cfg MCPConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return MCPConfig{}, fmt.Errorf("bootstrap: the MCP configuration %s: %w", path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return MCPConfig{}, fmt.Errorf("bootstrap: the MCP configuration %s: data after the object", path)
	}
	beside := func(name string) string {
		if filepath.IsAbs(name) {
			return name
		}
		return filepath.Join(filepath.Dir(path), name)
	}
	if cfg.PublicKeyFile == "" || cfg.CursorKeyFile == "" {
		return MCPConfig{}, errors.New("bootstrap: the MCP configuration names a public_key_file and a cursor_key_file")
	}
	if cfg.PublicKeys, err = os.ReadFile(beside(cfg.PublicKeyFile)); err != nil {
		return MCPConfig{}, fmt.Errorf("bootstrap: the gateway's public key: %w", err)
	}
	text, err := os.ReadFile(beside(cfg.CursorKeyFile))
	if err != nil {
		return MCPConfig{}, fmt.Errorf("bootstrap: the cursor key: %w", err)
	}
	if cfg.CursorKey, err = base64.StdEncoding.DecodeString(strings.TrimSpace(string(text))); err != nil {
		return MCPConfig{}, errors.New("bootstrap: the cursor key file holds base64")
	}
	return cfg, cfg.validate()
}

func (c MCPConfig) validate() error {
	u, err := url.Parse(c.Audience)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.Audience, "#") ||
		!strings.HasSuffix(u.Path, "/mcp"):
		return errors.New("bootstrap: the audience is the endpoint exactly as it is registered in idp-mcp: an http or https URL ending in /mcp")
	case !gatewayID.MatchString(c.GatewayID):
		return errors.New("bootstrap: the gateway_id is the id of the gateway whose assertions are accepted (letters, digits and . _ : -)")
	case len(c.CursorKey) < 32:
		return errors.New("bootstrap: the cursor key is 32 bytes or more")
	case c.Limits.CallsPerMinute < 0 || c.Limits.CallsPerMinute > 6000 || c.Limits.Concurrent < 0 || c.Limits.Concurrent > 64:
		return errors.New("bootstrap: the limits are 1 to 6000 calls a minute and 1 to 64 at once (zero: the defaults, 60 and 4)")
	}
	if _, err := mcptransport.NewVerifier(c.PublicKeys, c.Audience, c.GatewayID); err != nil {
		return err
	}
	return c.MCPPolicy.Validate()
}

// MCPServer is the handler of the MCP endpoint, and the ids of the gateway keys it holds, which the process logs at
// start.
type MCPServer struct {
	Handler http.Handler
	KeyIDs  []string
}

// MCP assembles the MCP server: FIN with FUL's and PHY's facts, which it reads, and its three read-only tools, behind
// the verifier of the gateway's assertion. FIN authorizes each query by the permissions of the principal the tools put
// on the context for the member the assertion names; a tool that is called without a verified caller refuses.
func (p *Platform) MCP(cfg MCPConfig, logger *slog.Logger) (MCPServer, error) {
	if err := cfg.validate(); err != nil {
		return MCPServer{}, err
	}
	verifier, err := mcptransport.NewVerifier(cfg.PublicKeys, cfg.Audience, cfg.GatewayID)
	if err != nil {
		return MCPServer{}, err
	}
	org, err := p.ORG(permissions{})
	if err != nil {
		return MCPServer{}, err
	}
	rate, err := p.RATE(org, permissions{}, margins{floor: 2000})
	if err != nil {
		return MCPServer{}, err
	}
	com, err := p.COM(org, rate, permissions{})
	if err != nil {
		return MCPServer{}, err
	}
	ful, phy, err := p.FULAndPHY(com, permissions{}, permissions{})
	if err != nil {
		return MCPServer{}, err
	}
	var auth finapi.Authorizer = permissions{}
	if cfg.authorizer != nil {
		auth = cfg.authorizer
	}
	// no tool issues a debit note, the only use FIN has for DOC's snapshots, so they stay unbound
	module, err := p.FIN(org, rate, ful, &DocumentSnapshots{}, phy, auth)
	if err != nil {
		return MCPServer{}, err
	}
	server := sdk.NewServer(&sdk.Implementation{Name: "idp-cargo-fin", Version: "v2.101"}, nil)
	err = module.RegisterMCP(server, fin.MCPConfig{
		Caller: func(ctx context.Context) (fin.MCPSubject, bool) {
			c, ok := mcptransport.CallerFrom(ctx)
			return fin.MCPSubject{Tenant: c.Tenant, ID: c.Subject, Roles: c.Roles, Client: c.Client, Actor: c.Actor}, ok
		},
		Policy:    cfg.MCPPolicy,
		CursorKey: cfg.CursorKey,
		Limits:    fin.MCPLimits{PerMinute: cfg.Limits.CallsPerMinute, Concurrent: cfg.Limits.Concurrent},
		Log:       logger,
	})
	if err != nil {
		return MCPServer{}, err
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", mcptransport.NewHandler(server, verifier, logger))
	return MCPServer{Handler: mux, KeyIDs: verifier.KeyIDs()}, nil
}
