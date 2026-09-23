package provider

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
)

// fakeAPI is an in-memory stand-in for the subset of `/v1` the provider
// calls, keeping the wire shapes crates/core-server/openapi/public.json
// documents.
type fakeAPI struct {
	mu sync.Mutex

	// bearer is the one token the server takes.
	bearer      string
	workspaceID string
	agents      map[string]*fakeAgent
	skills      map[string]*fakeSkill
	vaults      map[string]*fakeVault
	providers   map[string]*fakeProvider
	// apiKeys records the model provider key each provider holds, by
	// provider id.
	apiKeys map[string]string
	// keyRotations counts the updates that carried a key, by provider id.
	keyRotations map[string]int
	// refuseProviderUpdates makes every model provider update answer 500,
	// for a test that needs one to fail.
	refuseProviderUpdates bool
	// secrets records every credential payload sent, by credential id.
	secrets map[string]map[string]any
	// securityPuts counts every allowlist replacement.
	securityPuts int
}

type fakeAgent struct {
	Agent    map[string]any
	Versions []map[string]any
	Origins  []string
}

type fakeSkill struct {
	ID          string
	DisplayName string
	Versions    []map[string]any
}

type fakeVault struct {
	Vault       map[string]any
	Credentials []map[string]any
}

type fakeProvider struct {
	Body map[string]any
}

func newFakeAPI(t *testing.T) (*fakeAPI, *httptest.Server) {
	t.Helper()
	api := &fakeAPI{
		bearer:       "sbk_ak_test",
		workspaceID:  newID(),
		agents:       map[string]*fakeAgent{},
		skills:       map[string]*fakeSkill{},
		vaults:       map[string]*fakeVault{},
		providers:    map[string]*fakeProvider{},
		apiKeys:      map[string]string{},
		keyRotations: map[string]int{},
		secrets:      map[string]map[string]any{},
	}
	api.providers[platformProviderID] = &fakeProvider{Body: map[string]any{
		"type": "platform", "id": platformProviderID, "format": "anthropic",
		"display_name": "Anthropic", "description": "", "created_at": "2026-01-01T00:00:00Z",
		"models": []any{map[string]any{"model": "claude-sonnet-5", "display_name": "Sonnet", "description": ""}},
	}}
	server := httptest.NewServer(http.HandlerFunc(api.serve))
	t.Cleanup(server.Close)
	return api, server
}

// platformProviderID is the platform provider these tests point agents at.
// Every id the server answers with is a UUID in its canonical spelling, and
// the provider takes no other.
const platformProviderID = "01a07f90-238c-7de3-b55e-6fe53de063cf"

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type route struct {
	method  string
	pattern *regexp.Regexp
	handle  func(a *fakeAPI, r *http.Request, args []string) (int, any)
}

func on(method, pattern string, handle func(a *fakeAPI, r *http.Request, args []string) (int, any)) route {
	return route{method: method, pattern: regexp.MustCompile("^" + pattern + "$"), handle: handle}
}

const seg = `([^/]+)`

var routes = []route{
	on("GET", "/v1/workspaces/"+seg, (*fakeAPI).getWorkspace),
	on("POST", "/v1/agents", (*fakeAPI).createAgent),
	on("GET", "/v1/agents/"+seg, (*fakeAPI).getAgent),
	on("PATCH", "/v1/agents/"+seg, (*fakeAPI).renameAgent),
	on("DELETE", "/v1/agents/"+seg, (*fakeAPI).deleteAgent),
	on("GET", "/v1/agents/"+seg+"/security", (*fakeAPI).getSecurity),
	on("PUT", "/v1/agents/"+seg+"/security", (*fakeAPI).putSecurity),
	on("POST", "/v1/agents/"+seg+"/versions", (*fakeAPI).publish),
	on("GET", "/v1/agents/"+seg+"/versions", (*fakeAPI).listAgentVersions),
	on("POST", "/v1/skills", (*fakeAPI).createSkill),
	on("GET", "/v1/skills/"+seg, (*fakeAPI).getSkill),
	on("PATCH", "/v1/skills/"+seg, (*fakeAPI).renameSkill),
	on("DELETE", "/v1/skills/"+seg, (*fakeAPI).deleteSkill),
	on("POST", "/v1/skills/"+seg+"/versions", (*fakeAPI).pushSkill),
	on("GET", "/v1/skills/"+seg+"/versions", (*fakeAPI).listSkillVersions),
	on("POST", "/v1/vaults", (*fakeAPI).createVault),
	on("GET", "/v1/vaults/"+seg, (*fakeAPI).getVault),
	on("PATCH", "/v1/vaults/"+seg, (*fakeAPI).updateVault),
	on("DELETE", "/v1/vaults/"+seg, (*fakeAPI).deleteVault),
	on("POST", "/v1/vaults/"+seg+"/credentials", (*fakeAPI).addCredential),
	on("GET", "/v1/vaults/"+seg+"/credentials", (*fakeAPI).listCredentials),
	on("DELETE", "/v1/vaults/"+seg+"/credentials/"+seg, (*fakeAPI).deleteCredential),
	on("POST", "/v1/model-providers", (*fakeAPI).createProvider),
	on("GET", "/v1/model-providers", (*fakeAPI).listProviders),
	on("GET", "/v1/model-providers/"+seg, (*fakeAPI).getProvider),
	on("PATCH", "/v1/model-providers/"+seg, (*fakeAPI).updateProvider),
	on("DELETE", "/v1/model-providers/"+seg, (*fakeAPI).deleteProvider),
}

func errBody(code, message string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": message}}
}

var notFound = errBody("not_found", "not found")

func (a *fakeAPI) serve(w http.ResponseWriter, req *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if req.Header.Get("Authorization") != "Bearer "+a.bearer {
		writeJSON(w, http.StatusUnauthorized, errBody("unauthorized", "bad token"))
		return
	}
	// As `WorkspaceScoped` does: a header is welcome on every route, and only
	// one naming another workspace is refused.
	if named := req.Header.Get("Kikuvi-Workspace"); named != "" && named != a.workspaceID {
		writeJSON(w, http.StatusForbidden, errBody("forbidden", "Kikuvi-Workspace names another workspace"))
		return
	}
	// Every public POST takes a key, and the provider sends one on each.
	if req.Method == http.MethodPost && req.Header.Get("Idempotency-Key") == "" {
		writeJSON(w, http.StatusBadRequest, errBody("invalid_request", "Idempotency-Key header is required"))
		return
	}
	for _, rt := range routes {
		if rt.method != req.Method {
			continue
		}
		m := rt.pattern.FindStringSubmatch(req.URL.Path)
		if m == nil {
			continue
		}
		status, body := rt.handle(a, req, m[1:])
		writeJSON(w, status, body)
		return
	}
	writeJSON(w, http.StatusNotFound, errBody("not_found", "no route "+req.Method+" "+req.URL.Path))
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	if body == nil {
		w.WriteHeader(status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func decode(r *http.Request) map[string]any {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body
}

// page answers the newest-first single-item page the provider asks for, or
// every item for a full walk.
func page(r *http.Request, items []map[string]any) map[string]any {
	out := append([]map[string]any(nil), items...)
	if r.URL.Query().Get("order") == "desc" {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	if r.URL.Query().Get("limit") == "1" && len(out) > 1 {
		out = out[:1]
	}
	return map[string]any{"items": out, "next_cursor": nil}
}

func (a *fakeAPI) getWorkspace(_ *http.Request, args []string) (int, any) {
	if args[0] != a.workspaceID {
		return 404, notFound
	}
	return 200, map[string]any{"id": a.workspaceID, "organization_id": "org", "name": "acme", "created_at": "2026-01-01T00:00:00Z"}
}

func (a *fakeAPI) createAgent(r *http.Request, _ []string) (int, any) {
	body := decode(r)
	agentID := newID()
	a.agents[agentID] = &fakeAgent{
		Agent:   map[string]any{"id": agentID, "workspace_id": a.workspaceID, "name": body["name"], "created_at": "2026-01-01T00:00:00Z"},
		Origins: []string{},
	}
	return 201, a.agents[agentID].Agent
}

func (a *fakeAPI) getAgent(_ *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, agent.Agent
}

func (a *fakeAPI) renameAgent(r *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	agent.Agent["name"] = decode(r)["name"]
	return 200, agent.Agent
}

func (a *fakeAPI) deleteAgent(_ *http.Request, args []string) (int, any) {
	if _, ok := a.agents[args[0]]; !ok {
		return 404, notFound
	}
	delete(a.agents, args[0])
	return 204, nil
}

func (a *fakeAPI) getSecurity(_ *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, map[string]any{"allowed_origins": agent.Origins}
}

func (a *fakeAPI) putSecurity(r *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	var body struct {
		AllowedOrigins []string `json:"allowed_origins"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	a.securityPuts++
	agent.Origins = append([]string{}, body.AllowedOrigins...)
	return 200, map[string]any{"allowed_origins": agent.Origins}
}

// storedConfig turns a publish body into the form a version stores: the
// model's settings under `config`, empty grant lists dropped.
func storedConfig(body map[string]any) map[string]any {
	model := body["model"].(map[string]any)
	settings := map[string]any{}
	for k, v := range model {
		if k != "format" && v != nil {
			settings[k] = v
		}
	}
	config := map[string]any{"model": map[string]any{"format": model["format"], "config": settings}}
	if prompt, ok := body["system_prompt"]; ok && prompt != nil {
		config["system_prompt"] = prompt
	}
	for _, key := range []string{"mcp", "skills"} {
		if list, ok := body[key].([]any); ok && len(list) > 0 {
			config[key] = list
		}
	}
	return config
}

func (a *fakeAPI) publish(r *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	body := decode(r)
	version := map[string]any{
		"id":                newID(),
		"agent_id":          args[0],
		"version":           len(agent.Versions) + 1,
		"model_provider_id": body["model_provider_id"],
		"config":            storedConfig(body),
		"created_at":        "2026-01-01T00:00:00Z",
	}
	agent.Versions = append(agent.Versions, version)
	return 201, version
}

func (a *fakeAPI) listAgentVersions(r *http.Request, args []string) (int, any) {
	agent, ok := a.agents[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, page(r, agent.Versions)
}

var frontmatterName = regexp.MustCompile(`(?m)^name:\s*(\S+)\s*$`)

// unpack reads an upload the way the server does: every file, its digest,
// and the frontmatter name SKILL.md declares.
func unpack(body []byte) (manifest []any, name string, err error) {
	gz, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	tr := tar.NewReader(gz)
	type file struct {
		path   string
		size   int
		digest string
	}
	var files []file
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, "", err
		}
		content, _ := io.ReadAll(tr)
		sum := sha256.Sum256(content)
		files = append(files, file{path: h.Name, size: len(content), digest: hex.EncodeToString(sum[:])})
		if h.Name == "SKILL.md" {
			if m := frontmatterName.FindSubmatch(content); m != nil {
				name = string(m[1])
			}
		}
	}
	if name == "" {
		return nil, "", fmt.Errorf("no frontmatter name")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].path < files[j].path })
	for _, f := range files {
		manifest = append(manifest, map[string]any{"path": f.path, "size": f.size, "sha256": f.digest})
	}
	return manifest, name, nil
}

func (a *fakeAPI) pushVersion(skill *fakeSkill, r *http.Request) (int, any) {
	if r.Header.Get("Content-Type") != "application/gzip" {
		return 400, errBody("invalid_request", "not gzip")
	}
	body, _ := io.ReadAll(r.Body)
	manifest, name, err := unpack(body)
	if err != nil {
		return 400, errBody("invalid_request", err.Error())
	}
	version := map[string]any{
		"id":           newID(),
		"skill_id":     skill.ID,
		"version":      len(skill.Versions) + 1,
		"frontmatter":  map[string]any{"name": name, "description": "d"},
		"manifest":     manifest,
		"bundle_bytes": len(body),
	}
	skill.Versions = append(skill.Versions, version)
	return 201, version
}

func (a *fakeAPI) createSkill(r *http.Request, _ []string) (int, any) {
	skill := &fakeSkill{ID: newID()}
	status, body := a.pushVersion(skill, r)
	if status == 201 {
		a.skills[skill.ID] = skill
	}
	return status, body
}

func (a *fakeAPI) pushSkill(r *http.Request, args []string) (int, any) {
	skill, ok := a.skills[args[0]]
	if !ok {
		return 404, notFound
	}
	return a.pushVersion(skill, r)
}

func (s *fakeSkill) body() map[string]any {
	latest := s.Versions[len(s.Versions)-1]
	return map[string]any{
		"id":             s.ID,
		"display_name":   s.DisplayName,
		"name":           latest["frontmatter"].(map[string]any)["name"],
		"latest_version": latest["version"],
	}
}

func (a *fakeAPI) getSkill(_ *http.Request, args []string) (int, any) {
	skill, ok := a.skills[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, skill.body()
}

func (a *fakeAPI) renameSkill(r *http.Request, args []string) (int, any) {
	skill, ok := a.skills[args[0]]
	if !ok {
		return 404, notFound
	}
	skill.DisplayName, _ = decode(r)["display_name"].(string)
	return 200, skill.body()
}

// grantedSkill reports whether any agent version grants skillID, which the
// server refuses to delete under.
func (a *fakeAPI) grantedSkill(skillID string) bool {
	for _, agent := range a.agents {
		for _, v := range agent.Versions {
			encoded, _ := json.Marshal(v["config"])
			if strings.Contains(string(encoded), skillID) {
				return true
			}
		}
	}
	return false
}

func (a *fakeAPI) deleteSkill(_ *http.Request, args []string) (int, any) {
	if _, ok := a.skills[args[0]]; !ok {
		return 404, notFound
	}
	if a.grantedSkill(args[0]) {
		return 409, errBody("conflict", "skill is still granted")
	}
	delete(a.skills, args[0])
	return 204, nil
}

func (a *fakeAPI) listSkillVersions(r *http.Request, args []string) (int, any) {
	skill, ok := a.skills[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, page(r, skill.Versions)
}

func (a *fakeAPI) createVault(r *http.Request, _ []string) (int, any) {
	body := decode(r)
	vaultID := newID()
	a.vaults[vaultID] = &fakeVault{Vault: map[string]any{
		"id": vaultID, "workspace_id": a.workspaceID, "display_name": body["display_name"],
		"metadata": body["metadata"], "created_at": "2026-01-01T00:00:00Z",
	}}
	return 201, map[string]any{"id": vaultID}
}

func (a *fakeAPI) getVault(_ *http.Request, args []string) (int, any) {
	vault, ok := a.vaults[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, vault.Vault
}

func (a *fakeAPI) updateVault(r *http.Request, args []string) (int, any) {
	vault, ok := a.vaults[args[0]]
	if !ok {
		return 404, notFound
	}
	for k, v := range decode(r) {
		if v != nil {
			vault.Vault[k] = v
		}
	}
	return 200, vault.Vault
}

func (a *fakeAPI) deleteVault(_ *http.Request, args []string) (int, any) {
	if _, ok := a.vaults[args[0]]; !ok {
		return 404, notFound
	}
	delete(a.vaults, args[0])
	return 204, nil
}

func (a *fakeAPI) addCredential(r *http.Request, args []string) (int, any) {
	vault, ok := a.vaults[args[0]]
	if !ok {
		return 404, notFound
	}
	body := decode(r)
	for _, c := range vault.Credentials {
		if c["target"] == body["target"] {
			return 409, errBody("conflict", "credential target already exists")
		}
	}
	payload := body["payload"].(map[string]any)
	credentialID := newID()
	vault.Credentials = append(vault.Credentials, map[string]any{
		"id": credentialID, "vault_id": args[0], "protocol": body["protocol"],
		"auth_scheme": payload["auth_scheme"], "target": body["target"],
		"display_name": body["display_name"],
	})
	a.secrets[credentialID] = payload
	return 201, map[string]any{"id": credentialID}
}

func (a *fakeAPI) listCredentials(r *http.Request, args []string) (int, any) {
	vault, ok := a.vaults[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, page(r, vault.Credentials)
}

func (a *fakeAPI) deleteCredential(_ *http.Request, args []string) (int, any) {
	vault, ok := a.vaults[args[0]]
	if !ok {
		return 404, notFound
	}
	for i, c := range vault.Credentials {
		if c["id"] == args[1] {
			vault.Credentials = append(vault.Credentials[:i], vault.Credentials[i+1:]...)
			return 204, nil
		}
	}
	return 404, notFound
}

func (a *fakeAPI) createProvider(r *http.Request, _ []string) (int, any) {
	if r.Header.Get("Kikuvi-Workspace") != a.workspaceID {
		return 400, errBody("invalid_request", "Kikuvi-Workspace header is required")
	}
	body := decode(r)
	providerID := newID()
	a.apiKeys[providerID], _ = body["api_key"].(string)
	a.providers[providerID] = &fakeProvider{Body: map[string]any{
		"type": "byok", "id": providerID, "format": body["format"], "workspace_id": a.workspaceID,
		"display_name": body["display_name"], "description": body["description"],
		"created_at": "2026-01-01T00:00:00Z",
	}}
	return 201, a.providers[providerID].Body
}

func (a *fakeAPI) listProviders(r *http.Request, _ []string) (int, any) {
	var items []map[string]any
	for _, key := range sortedKeys(a.providers) {
		items = append(items, a.providers[key].Body)
	}
	return 200, page(r, items)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (a *fakeAPI) getProvider(r *http.Request, args []string) (int, any) {
	if r.Header.Get("Kikuvi-Workspace") != a.workspaceID {
		return 400, errBody("invalid_request", "Kikuvi-Workspace header is required")
	}
	p, ok := a.providers[args[0]]
	if !ok {
		return 404, notFound
	}
	return 200, p.Body
}

// updateProvider applies `UpdateModelProviderBody`: an absent field leaves
// that part of the provider as it stands. Neither the base URL nor the key is
// ever returned, so neither joins the body this answers with.
func (a *fakeAPI) updateProvider(r *http.Request, args []string) (int, any) {
	p, ok := a.providers[args[0]]
	if !ok {
		return 404, notFound
	}
	if a.refuseProviderUpdates {
		return 500, map[string]any{"error": map[string]any{"code": "internal", "message": "refused"}}
	}
	body := decode(r)
	if key, sent := body["api_key"].(string); sent {
		a.apiKeys[args[0]] = key
		a.keyRotations[args[0]]++
	}
	for k, v := range body {
		if v == nil || k == "api_key" || k == "base_url" {
			continue
		}
		p.Body[k] = v
	}
	return 200, p.Body
}

// seedProvider puts a workspace model provider on the server that no
// configuration has ever managed, the way an import's subject arrives.
func (a *fakeAPI) seedProvider(format, displayName, key string) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	providerID := newID()
	a.apiKeys[providerID] = key
	a.providers[providerID] = &fakeProvider{Body: map[string]any{
		"type": "byok", "id": providerID, "format": format, "workspace_id": a.workspaceID,
		"display_name": displayName, "description": "", "created_at": "2026-01-01T00:00:00Z",
	}}
	return providerID
}

func (a *fakeAPI) deleteProvider(_ *http.Request, args []string) (int, any) {
	if _, ok := a.providers[args[0]]; !ok {
		return 404, notFound
	}
	delete(a.providers, args[0])
	return 204, nil
}
