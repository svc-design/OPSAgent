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
)

type AlertmanagerWebhook struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

func main() {
	dsn := getenv("DB_DSN", "postgres://postgres:password@localhost:5432/observability?sslmode=disable")
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer conn.Close(context.Background())

	http.HandleFunc("/webhook", func(w http.ResponseWriter, r *http.Request) {
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
		verifyAndClose(context.Background(), conn, id)
		w.Write([]byte(prURL))
	})

	log.Println("agent listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func createPR(desc string) (string, error) {
	token := os.Getenv("GITHUB_TOKEN")
	repo := os.Getenv("GITHUB_REPO")
	owner, name := splitRepo(repo)
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	body := readTemplate()
	newPR := &github.NewPullRequest{
		Title: github.String(desc),
		Head:  github.String("main"),
		Base:  github.String("main"),
		Body:  github.String(body),
	}
	pr, _, err := client.PullRequests.Create(ctx, owner, name, newPR)
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

func verifyAndClose(ctx context.Context, conn *pgx.Conn, id int) {
	var avg float64
	if err := conn.QueryRow(ctx, "SELECT COALESCE(avg(avg_value),0) FROM metrics_1m WHERE name='cpu' AND bucket > now() - interval '5 minutes'").Scan(&avg); err != nil {
		log.Printf("verify incident: %v", err)
		return
	}
	if avg < 80 {
		if _, err := conn.Exec(ctx, "UPDATE incidents SET status='closed' WHERE id=$1", id); err != nil {
			log.Printf("close incident: %v", err)
		}
	}
}

func checkArgoCD() {
	url := os.Getenv("ARGOCD_URL")
	app := os.Getenv("ARGOCD_APP")
	if url == "" || app == "" {
		return
	}
	resp, err := http.Get(fmt.Sprintf("%s/api/v1/applications/%s", url, app))
	if err != nil {
		log.Printf("argocd health: %v", err)
		return
	}
	defer resp.Body.Close()
	log.Printf("argocd status: %s", resp.Status)
}

func splitRepo(repo string) (string, string) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 {
		return "", ""
	}
	return parts[0], parts[1]
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
