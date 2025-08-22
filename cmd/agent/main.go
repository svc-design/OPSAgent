package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/google/go-github/v55/github"
	"github.com/jackc/pgx/v5"
	"golang.org/x/oauth2"
	"gopkg.in/yaml.v3"
)

type AlertmanagerWebhook struct {
	CommonLabels map[string]string `json:"commonLabels"`
	Alerts       []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

type Response struct {
	IncidentID int    `json:"incident_id"`
	PRURL      string `json:"pr_url"`
	Verified   bool   `json:"verified"`
}

func main() {
	dsn := getenv("PG_URL", "postgres://postgres:postgres@127.0.0.1:5432/ops?sslmode=disable")
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer conn.Close(context.Background())

	listen := getenv("LISTEN_ADDR", ":8080")

	http.HandleFunc("/alertmanager", func(w http.ResponseWriter, r *http.Request) {
		var hook AlertmanagerWebhook
		if err := json.NewDecoder(r.Body).Decode(&hook); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(hook.Alerts) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		desc := hook.Alerts[0].Annotations["summary"]
		svc := hook.CommonLabels["service"]
		var id int
		if err := conn.QueryRow(context.Background(),
			"INSERT INTO incidents (description) VALUES ($1) RETURNING id", desc).Scan(&id); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		prURL, err := createPR(desc)
		if err != nil {
			log.Printf("create PR: %v", err)
		}
		checkArgoCD()
		verified := verifyAndClose(context.Background(), conn, id, svc)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(Response{IncidentID: id, PRURL: prURL, Verified: verified})
	})

	log.Printf("agent listening on %s", listen)
	log.Fatal(http.ListenAndServe(listen, nil))
}

func createPR(desc string) (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	owner := os.Getenv("GITHUB_OWNER")
	repo := os.Getenv("GITHUB_REPO")
	base := getenv("GITHUB_BASE_BRANCH", "main")
	file := os.Getenv("GITHUB_FILE_PATH")
	flag := os.Getenv("FLAG_PATH")
	if file != "" && flag != "" {
		toggleFlag(file, flag)
	}
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	body := readTemplate()
	newPR := &github.NewPullRequest{
		Title: github.String(desc),
		Head:  github.String(base),
		Base:  github.String(base),
		Body:  github.String(body),
	}
	pr, _, err := client.PullRequests.Create(ctx, owner, repo, newPR)
	if err != nil {
		return "", err
	}
	return pr.GetHTMLURL(), nil
}

func readTemplate() string {
	b, err := os.ReadFile("configs/github/pr_template.md")
	if err != nil {
		return ""
	}
	return string(b)
}

func toggleFlag(file, path string) {
	b, err := os.ReadFile(file)
	if err != nil {
		return
	}
	var data map[string]interface{}
	if err := yaml.Unmarshal(b, &data); err != nil {
		return
	}
	parts := strings.Split(path, ".")
	m := data
	for i, p := range parts {
		if i == len(parts)-1 {
			m[p] = false
		} else {
			next, ok := m[p].(map[string]interface{})
			if !ok {
				next = make(map[string]interface{})
				m[p] = next
			}
			m = next
		}
	}
	out, err := yaml.Marshal(data)
	if err != nil {
		return
	}
	_ = os.WriteFile(file, out, 0644)
}

func verifyAndClose(ctx context.Context, conn *pgx.Conn, id int, svc string) bool {
	var improved bool
	if err := conn.QueryRow(ctx, "SELECT recent_latency_improved($1)", svc).Scan(&improved); err != nil {
		log.Printf("verify incident: %v", err)
		return false
	}
	if improved {
		if _, err := conn.Exec(ctx, "UPDATE incidents SET status='closed' WHERE id=$1", id); err != nil {
			log.Printf("close incident: %v", err)
		}
	}
	return improved
}

func checkArgoCD() {
	url := os.Getenv("ARGOCD_URL")
	app := os.Getenv("ARGOCD_APP")
	token := os.Getenv("ARGOCD_TOKEN")
	if url == "" || app == "" {
		return
	}
	req, err := http.NewRequest("GET", fmt.Sprintf("%s/api/v1/applications/%s", url, app), nil)
	if err != nil {
		log.Printf("argocd request: %v", err)
		return
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("argocd health: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("argocd status: %s", resp.Status)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
