package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/hann0w0/singbox-panel/internal/config"
	"github.com/hann0w0/singbox-panel/internal/domain/model"
)

func TestSubscriptionHistoryCountsFullWindowAndPaginates(t *testing.T) {
	db := testDB(t)
	user := model.User{Email: "history", SubToken: "history"}
	mustCreate(t, db, &user)
	now := time.Now().UTC().Truncate(time.Millisecond)
	rows := make([]model.SubscriptionFetch, 5005)
	for i := range rows {
		rows[i] = model.SubscriptionFetch{UserID: user.ID, IP: "1.1.1.1", Client: "old", Format: "clash", Status: 200, CreatedAt: now.Add(-2 * time.Hour)}
	}
	rows[0].Status = 503
	// Newest timestamp, not largest ID, determines the displayed client.
	rows[1].CreatedAt, rows[1].Client = now.Add(-time.Minute), "newest"
	if err := db.CreateInBatches(&rows, 200).Error; err != nil {
		t.Fatal(err)
	}
	for _, row := range []model.SubscriptionFetch{
		{UserID: user.ID, IP: "8.8.8.8", Client: "Surge", Status: 204, CreatedAt: now.Add(-2 * 24 * time.Hour)},
		{UserID: user.ID, IP: "1.1.1.1", Status: 500, CreatedAt: now.Add(-31 * 24 * time.Hour)},
		{UserID: user.ID + 1, IP: "1.1.1.1", Status: 500, CreatedAt: now},
	} {
		mustCreate(t, db, &row)
	}
	app := &App{db: db}
	router := gin.New()
	router.GET("/users/:id/sub-fetches", app.listUserSubscriptionFetches)
	path := fmt.Sprintf("/users/%d/sub-fetches", user.ID)
	get := func(query string, out any) int {
		t.Helper()
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path+query, nil))
		if out != nil && json.Unmarshal(rec.Body.Bytes(), out) != nil {
			t.Fatalf("invalid response: %s", rec.Body.String())
		}
		return rec.Code
	}
	var summary struct {
		IPs   []subscriptionIPSummary `json:"ips"`
		Total int64                   `json:"total"`
		Until string                  `json:"until"`
	}
	if status := get("?range=30d", &summary); status != 200 || summary.Total != 5006 || len(summary.IPs) != 2 {
		t.Fatalf("summary: %d %+v", status, summary)
	}
	if first := summary.IPs[0]; first.Fetches != 5005 || first.Failures != 1 || first.Client != "newest" || first.ClientCount != 2 {
		t.Fatalf("truncated or incorrectly ordered summary: %+v", first)
	}
	until := summary.Until
	if status := get("?range=24h", &summary); status != 200 || summary.Total != 5005 || len(summary.IPs) != 1 {
		t.Fatalf("24-hour summary: %d %+v", status, summary)
	}
	// A later download cannot shift pages within the captured snapshot.
	late := model.SubscriptionFetch{UserID: user.ID, IP: "1.1.1.1", Status: 200, CreatedAt: time.Now().Add(time.Second)}
	mustCreate(t, db, &late)
	var details struct {
		Fetches []model.SubscriptionFetch `json:"fetches"`
		Next    string                    `json:"next_cursor"`
	}
	query := "?range=30d&ip=1.1.1.1&limit=6&until=" + url.QueryEscape(until)
	if status := get(query, &details); status != 200 || len(details.Fetches) != 6 || details.Fetches[0].ID != rows[1].ID || details.Next == "" {
		t.Fatalf("first page: %d %+v", status, details)
	}
	seen := map[uint]bool{}
	for _, row := range details.Fetches {
		seen[row.ID] = true
	}
	if status := get(query+"&before="+url.QueryEscape(details.Next), &details); status != 200 || len(details.Fetches) != 6 {
		t.Fatalf("next page: %d %+v", status, details)
	}
	for _, row := range details.Fetches {
		if seen[row.ID] || row.ID == late.ID || row.UserID != user.ID {
			t.Fatalf("duplicate or foreign detail: %+v", row)
		}
	}
	for _, query := range []string{"?range=bad", "?ip=x&limit=0", "?ip=x&limit=51", "?ip=x&before=bad", "?ip=x&before=" + strings.Repeat("a", 257), "?until=bad"} {
		if status := get(query, nil); status != 400 {
			t.Fatalf("invalid query %s: %d", query, status)
		}
	}
	if status := get("?ip=missing", &details); status != 200 || len(details.Fetches) != 0 || details.Next != "" {
		t.Fatalf("empty details: %d %+v", status, details)
	}
}

func TestSubscriptionHistoryWindowRespectsRetention(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, age := range []time.Duration{0, 10 * 24 * time.Hour, 29 * 24 * time.Hour} {
		until := now.Add(-age)
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/?range=30d&until="+url.QueryEscape(until.Format(time.RFC3339)), nil)
		since, gotUntil, ok := subscriptionHistoryWindow(c, now)
		if !ok || !since.Equal(now.Add(-subscriptionFetchRetention)) || !gotUntil.Equal(until) {
			t.Fatalf("window exceeds retention: %v %v %v", since, gotUntil, ok)
		}
	}
}

func TestSubscriptionHistoryAdminAuthorization(t *testing.T) {
	db := testDB(t)
	user := model.User{Email: "reader", SubToken: "reader", Role: model.RoleUser, Enabled: true}
	mustCreate(t, db, &user)
	cfg := config.Default()
	cfg.Environment = "development"
	app := NewApp(cfg, db)
	token, err := app.auth.Issue(&user, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"sub-fetches", "sub-fetch-location?ip=1.1.1.1"} {
		for _, auth := range []string{"", "Bearer " + token} {
			req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/users/%d/%s", user.ID, endpoint), nil)
			req.Header.Set("Authorization", auth)
			rec := httptest.NewRecorder()
			app.engine.ServeHTTP(rec, req)
			want := 401
			if auth != "" {
				want = 403
			}
			if rec.Code != want {
				t.Fatalf("%s authorization: %d, want %d", endpoint, rec.Code, want)
			}
		}
	}
}

func TestSubscriptionHistoryIndexUpgrade(t *testing.T) {
	db := testDB(t)
	if err := db.Migrator().DropIndex(&model.SubscriptionFetch{}, "idx_sub_fetch_user_ip_time"); err != nil {
		t.Fatal(err)
	}
	for _, migration := range applicationMigrations {
		if migration.version == 27 {
			for i := 0; i < 2; i++ {
				if err := migration.up(db); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if !db.Migrator().HasIndex(&model.SubscriptionFetch{}, "idx_sub_fetch_user_ip_time") {
		t.Fatal("missing history index")
	}
}
