package main

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

// ---------- Config ----------
type Config struct {
	PGURL         string
	ListenAddr    string
	GitHubToken   string
	GitHubOwner   string
	GitHubRepo    string
	GitBaseBranch string
	GitFilePath   string // path to values.yaml that holds feature flag
	FlagPath      string // dot.path.to.flag to toggle
	ArgoCDURL     string
	ArgoCDToken   string
	ArgoCDApp     string
}

func mustEnv(key, def string) string {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	return v
}

func cfgFromEnv() Config {
	return Config{
		PGURL:         mustEnv("PG_URL", "postgres://postgres:postgres@127.0.0.1:5432/ops?sslmode=disable"),
		ListenAddr:    mustEnv("LISTEN_ADDR", ":8080"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		GitHubOwner:   os.Getenv("GITHUB_OWNER"),
		GitHubRepo:    os.Getenv("GITHUB_REPO"),
		GitBaseBranch: mustEnv("GITHUB_BASE_BRANCH", "main"),
		GitFilePath:   mustEnv("GITHUB_FILE_PATH", "charts/app/values.yaml"),
		FlagPath:      mustEnv("FLAG_PATH", "featureFlags.recommendation_v2"),
		ArgoCDURL:     os.Getenv("ARGOCD_URL"),
		ArgoCDToken:   os.Getenv("ARGOCD_TOKEN"),
		ArgoCDApp:     os.Getenv("ARGOCD_APP"),
	}
}

// ---------- DB ----------
type DB struct{ pool *pgxpool.Pool }

func NewDB(ctx context.Context, url string) (*DB, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) UpsertIncident(ctx context.Context, fp, service string, sev int, hypothesis map[string]any) (int64, error) {
	var id int64
	err := d.pool.QueryRow(ctx, `
        INSERT INTO incidents(fingerprint, service, status, sev, hypothesis)
        VALUES($1,$2,'open',$3,$4)
        ON CONFLICT(fingerprint) DO UPDATE SET service=EXCLUDED.service
        RETURNING id;
    `, fp, service, sev, hypothesis).Scan(&id)
	return id, err
}

func (d *DB) AddAudit(ctx context.Context, incID int64, step int, action string, params, result map[string]any, dry bool) error {
	_, err := d.pool.Exec(ctx, `
        INSERT INTO actions_audit(incident_id, step, action, params, result, dryrun)
        VALUES($1,$2,$3,$4,$5,$6);
    `, incID, step, action, params, result, dry)
	return err
}

func (d *DB) CloseIncidentIfStable(ctx context.Context, incID int64, service string) (bool, error) {
	// Compare avg latency last 5m vs prev 5m; consider success if reduced by >=10%.
	var recent, prev sql.NullFloat64
	if err := d.pool.QueryRow(ctx, `SELECT recent_latency_avg($1, 5)`, service).Scan(&recent); err != nil {
		return false, err
	}
	if err := d.pool.QueryRow(ctx, `
        SELECT avg(avg_val) FROM metrics_1m
        WHERE service=$1 AND metric='http_req_latency'
        AND tb >= now()-interval '10 minutes' AND tb < now()-interval '5 minutes'`, service).Scan(&prev); err != nil {
		return false, err
	}
	if !recent.Valid || !prev.Valid {
		return false, errors.New("insufficient data")
	}
	if prev.Float64 <= 0 {
		return false, errors.New("invalid previous baseline")
	}
	reduction := (prev.Float64 - recent.Float64) / prev.Float64
	if reduction >= 0.10 {
		_, err := d.pool.Exec(ctx, `UPDATE incidents SET status='resolved', closed_at=now() WHERE id=$1`, incID)
		return err == nil, err
	}
	return false, nil
}

// ---------- Alertmanager payload ----------
type Alert struct {
	Status       string            `json:"status"`
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

func fingerprintFromAlert(a Alert) string {
	// simple PoC fingerprint
	b, _ := json.Marshal(a.CommonLabels)
	return base64.StdEncoding.EncodeToString(b)
}

// ---------- GitHub minimal client (REST) ----------
type githubClient struct {
	owner, repo, token, baseBranch string
}

func (g *githubClient) do(ctx context.Context, method, url string, body io.Reader) (*http.Response, error) {
	client := &http.Client{Timeout: 15 * time.Second}
	req, _ := http.NewRequestWithContext(ctx, method, url, body)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+g.token)
	return client.Do(req)
}

func (g *githubClient) getRef(ctx context.Context, branch string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/ref/heads/%s", g.owner, g.repo, branch)
	resp, err := g.do(ctx, "GET", url, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("getRef status=%d", resp.StatusCode)
	}
	var out struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Object.SHA, nil
}

func (g *githubClient) createRef(ctx context.Context, newBranch, fromSHA string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/git/refs", g.owner, g.repo)
	payload := map[string]any{"ref": "refs/heads/" + newBranch, "sha": fromSHA}
	b, _ := json.Marshal(payload)
	resp, err := g.do(ctx, "POST", url, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("createRef status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func (g *githubClient) getFile(ctx context.Context, path, ref string) (contentB64, sha string, err error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s?ref=%s", g.owner, g.repo, path, ref)
	resp, err := g.do(ctx, "GET", url, nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("getFile status=%d", resp.StatusCode)
	}
	var out struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	return out.Content, out.SHA, nil
}

func (g *githubClient) putFile(ctx context.Context, path, branch, message, newContentB64, sha string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/contents/%s", g.owner, g.repo, path)
	payload := map[string]any{
		"message": message,
		"content": newContentB64,
		"branch":  branch,
		"sha":     sha,
	}
	b, _ := json.Marshal(payload)
	resp, err := g.do(ctx, "PUT", url, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 && resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("putFile status=%d body=%s", resp.StatusCode, string(body))
	}
	return nil
}

func (g *githubClient) createPR(ctx context.Context, title, head, base, body string) (string, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/%s/pulls", g.owner, g.repo)
	payload := map[string]string{"title": title, "head": head, "base": base, "body": body}
	b, _ := json.Marshal(payload)
	resp, err := g.do(ctx, "POST", url, strings.NewReader(string(b)))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 201 {
		bdy, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("createPR status=%d body=%s", resp.StatusCode, string(bdy))
	}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// Toggle a boolean flag in YAML via dot path
func toggleFlagInYAML(src []byte, dotPath string, desired bool) ([]byte, error) {
	var m map[string]any
	if err := yaml.Unmarshal(src, &m); err != nil {
		return nil, err
	}
	parts := strings.Split(dotPath, ".")
	var cur any = m
	for i, p := range parts {
		if i == len(parts)-1 {
			if mm, ok := cur.(map[string]any); ok {
				mm[p] = desired
			} else {
				return nil, fmt.Errorf("path not found")
			}
		} else {
			mm, ok := cur.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("bad path")
			}
			nxt, ok := mm[p]
			if !ok {
				nxt = map[string]any{}
				mm[p] = nxt
			}
			cur = nxt
		}
	}
	return yaml.Marshal(m)
}

// ---------- ArgoCD poll (optional) ----------
func waitArgoCDHealthy(ctx context.Context, baseURL, token, app string, timeout time.Duration) error {
	if baseURL == "" || token == "" || app == "" {
		// Not configured in PoC, skip
		return nil
	}
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		url := strings.TrimRight(baseURL, "/") + "/api/v1/applications/" + app
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err == nil && resp.StatusCode == 200 {
			var out struct {
				Status struct {
					Sync struct {
						Status string `json:"status"`
					} `json:"sync"`
					Health struct {
						Status string `json:"status"`
					} `json:"health"`
				} `json:"status"`
			}
			json.NewDecoder(resp.Body).Decode(&out)
			resp.Body.Close()
			if strings.EqualFold(out.Status.Sync.Status, "Synced") && strings.EqualFold(out.Status.Health.Status, "Healthy") {
				return nil
			}
		}
		time.Sleep(5 * time.Second)
	}
	return errors.New("argocd app not healthy within timeout")
}

// ---------- Handler ----------
func handleAlert(db *DB, cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rand.Seed(time.Now().UnixNano())
		ctx := r.Context()
		var a Alert
		if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		service := a.CommonLabels["service"]
		if service == "" && len(a.Alerts) > 0 {
			service = a.Alerts[0].Labels["service"]
		}
		if service == "" {
			service = "unknown"
		}

		fp := fingerprintFromAlert(a)
		incID, err := db.UpsertIncident(ctx, fp, service, 2, map[string]any{"summary": "auto-generated"})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}

		// --- Build and submit PR ---
		gh := &githubClient{owner: cfg.GitHubOwner, repo: cfg.GitHubRepo, token: cfg.GitHubToken, baseBranch: cfg.GitBaseBranch}
		baseSHA, err := gh.getRef(ctx, cfg.GitBaseBranch)
		if err != nil {
			http.Error(w, "getRef: "+err.Error(), 500)
			return
		}
		branch := fmt.Sprintf("ops-inc-%d-%d", incID, rand.Intn(10000))
		if err := gh.createRef(ctx, branch, baseSHA); err != nil {
			http.Error(w, "createRef: "+err.Error(), 500)
			return
		}

		b64, sha, err := gh.getFile(ctx, cfg.GitFilePath, cfg.GitBaseBranch)
		if err != nil {
			http.Error(w, "getFile: "+err.Error(), 500)
			return
		}
		// GitHub contents API returns base64 with newlines; strip them before decoding
		orig, _ := base64.StdEncoding.DecodeString(strings.ReplaceAll(b64, "\n", ""))
		patched, err := toggleFlagInYAML(orig, cfg.FlagPath, false)
		if err != nil {
			http.Error(w, "toggleYAML: "+err.Error(), 500)
			return
		}
		newB64 := base64.StdEncoding.EncodeToString(patched)
		msg := fmt.Sprintf("chore(ops): auto-disable %s due to incident %d", cfg.FlagPath, incID)
		if err := gh.putFile(ctx, cfg.GitFilePath, branch, msg, newB64, sha); err != nil {
			http.Error(w, "putFile: "+err.Error(), 500)
			return
		}

		prURL, err := gh.createPR(ctx, fmt.Sprintf("Auto mitigation for %s (INC-%d)", service, incID), branch, cfg.GitBaseBranch,
			fmt.Sprintf("Disable `%s` to mitigate elevated latency for service **%s**.\n\nIncident: %d", cfg.FlagPath, service, incID))
		if err != nil {
			http.Error(w, "createPR: "+err.Error(), 500)
			return
		}

		_ = db.AddAudit(ctx, incID, 1, "github_pr", map[string]any{"branch": branch}, map[string]any{"url": prURL}, false)

		// --- Wait ArgoCD (optional) ---
		argoErr := waitArgoCDHealthy(ctx, cfg.ArgoCDURL, cfg.ArgoCDToken, cfg.ArgoCDApp, 5*time.Minute)
		_ = db.AddAudit(ctx, incID, 2, "argocd_wait", map[string]any{"app": cfg.ArgoCDApp}, map[string]any{"error": fmt.Sprint(argoErr)}, false)

		// --- Verify with metrics in Timescale ---
		ok, verr := db.CloseIncidentIfStable(ctx, incID, service)
		if verr != nil {
			_ = db.AddAudit(ctx, incID, 3, "verify", map[string]any{}, map[string]any{"error": verr.Error()}, false)
		} else {
			_ = db.AddAudit(ctx, incID, 3, "verify", map[string]any{}, map[string]any{"success": ok}, false)
		}

		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fmt.Sprintf(`{"incident_id":%d,"pr_url":%q,"verified":%v}`, incID, prURL, ok))
	}
}

func main() {
	cfg := cfgFromEnv()
	if cfg.GitHubToken == "" || cfg.GitHubOwner == "" || cfg.GitHubRepo == "" {
		log.Println("[WARN] GitHub env not fully set; PR creation will fail.")
	}
	ctx := context.Background()
	_ = ctx
	db, err := NewDB(context.Background(), cfg.PGURL)
	if err != nil {
		log.Fatalf("db: %v", err)
	}
	http.HandleFunc("/alertmanager", handleAlert(db, cfg))
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })

	log.Printf("OPS Agent listening on %s", cfg.ListenAddr)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, nil))
}
