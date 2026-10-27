package panel

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/hann0w0/singbox-panel/internal/domain/model"
)

type subscriptionIPSummary struct {
	IP          string    `json:"ip"`
	Client      string    `json:"client"`
	Format      string    `json:"format"`
	Fetches     int64     `json:"fetches"`
	Failures    int64     `json:"failures"`
	ClientCount int64     `json:"client_count"`
	LastSeen    time.Time `json:"last_seen"`
}

type subscriptionFetchCursor struct {
	At time.Time `json:"at"`
	ID uint      `json:"id"`
}

func subscriptionHistoryWindow(c *gin.Context, now time.Time) (time.Time, time.Time, bool) {
	duration := subscriptionFetchRetention
	switch c.DefaultQuery("range", "30d") {
	case "24h":
		duration = 24 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
	case "30d":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported history range"})
		return time.Time{}, time.Time{}, false
	}
	until := now.UTC()
	if raw := c.Query("until"); raw != "" {
		var err error
		until, err = time.Parse(time.RFC3339Nano, raw)
		if err != nil || until.After(now) || until.Before(now.Add(-subscriptionFetchRetention)) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid history window"})
			return time.Time{}, time.Time{}, false
		}
	}
	since := until.Add(-duration)
	if oldest := now.Add(-subscriptionFetchRetention); since.Before(oldest) {
		since = oldest
	}
	return since, until, true
}

func subscriptionFetchQuery(db *gorm.DB, userID uint, since, until time.Time) *gorm.DB {
	return db.Model(&model.SubscriptionFetch{}).Where("user_id = ? AND created_at >= ? AND created_at <= ?", userID, since, until)
}

// Counts cover the whole window. Details use a frozen window and keyset cursor,
// so new downloads do not shift rows between pages while an admin reads them.
func (a *App) listUserSubscriptionFetches(c *gin.Context) {
	userID, ok := uintParam(c, "id")
	if !ok {
		return
	}
	since, until, ok := subscriptionHistoryWindow(c, time.Now())
	if !ok {
		return
	}
	db := a.db.WithContext(c.Request.Context())
	var user model.User
	if err := db.Select("id").First(&user, userID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if ip, present := c.GetQuery("ip"); present {
		limit := 10
		if raw := c.Query("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 50 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid page size"})
				return
			}
		}
		if len(ip) > 64 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid IP address"})
			return
		}
		query := subscriptionFetchQuery(db, userID, since, until).Where("ip = ?", ip)
		if raw := c.Query("before"); raw != "" {
			if len(raw) > 256 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid history cursor"})
				return
			}
			var cursor subscriptionFetchCursor
			data, err := base64.RawURLEncoding.DecodeString(raw)
			if err != nil || json.Unmarshal(data, &cursor) != nil || cursor.ID == 0 || cursor.At.IsZero() {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid history cursor"})
				return
			}
			query = query.Where("created_at < ? OR (created_at = ? AND id < ?)", cursor.At, cursor.At, cursor.ID)
		}
		rows := []model.SubscriptionFetch{}
		if err := query.Order("created_at DESC, id DESC").Limit(limit + 1).Find(&rows).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load subscription history"})
			return
		}
		next := ""
		if len(rows) > limit {
			rows = rows[:limit]
			last := rows[len(rows)-1]
			data, _ := json.Marshal(subscriptionFetchCursor{At: last.CreatedAt, ID: last.ID})
			next = base64.RawURLEncoding.EncodeToString(data)
		}
		c.JSON(http.StatusOK, gin.H{"fetches": rows, "next_cursor": next, "until": until})
		return
	}

	// The indexed correlated lookup selects the latest record by time AND ID,
	// including concurrent inserts whose timestamp order differs from ID order.
	grouped := subscriptionFetchQuery(db, userID, since, until).Select(`ip, COUNT(*) AS fetches,
		SUM(CASE WHEN status >= 200 AND status < 300 THEN 0 ELSE 1 END) AS failures,
		COUNT(DISTINCT client) AS client_count,
		(SELECT latest.id FROM subscription_fetches AS latest
		 WHERE latest.user_id = ? AND latest.ip = subscription_fetches.ip
		 AND latest.created_at >= ? AND latest.created_at <= ?
		 ORDER BY latest.created_at DESC, latest.id DESC LIMIT 1) AS latest_id`, userID, since, until).Group("ip")
	ips := []subscriptionIPSummary{}
	if err := db.Table("(?) AS grouped", grouped).
		Select("grouped.ip, grouped.fetches, grouped.failures, grouped.client_count, latest.client, latest.format, latest.created_at AS last_seen").
		Joins("JOIN subscription_fetches AS latest ON latest.id = grouped.latest_id").
		Order("latest.created_at DESC, latest.id DESC").Limit(10001).Scan(&ips).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load subscription history"})
		return
	}
	if len(ips) > 10000 {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "IP 数量过多，请缩短查询时间范围"})
		return
	}
	var total int64
	for _, ip := range ips {
		total += ip.Fetches
	}
	c.JSON(http.StatusOK, gin.H{"ips": ips, "total": total, "until": until})
}
