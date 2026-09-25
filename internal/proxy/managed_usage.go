package proxy

import (
	"errors"
	"net/http"
	"strconv"
	"time"
)

type usageHour struct {
	Date        string `json:"date"`
	Hour        int    `json:"hour"`
	Count       int64  `json:"count"`
	Generations int64  `json:"generations"`
}

func (s *stateDB) recordGeneration(completed time.Time, images int) error {
	if images < 1 || images > 4 {
		return errors.New("invalid generated image count")
	}
	quarter := completed.Unix() / 900 * 900
	_, err := s.db.Exec("INSERT INTO usage_quarters(quarter_start,generations,images) VALUES(?,1,?) ON CONFLICT(quarter_start) DO UPDATE SET generations=generations+1,images=images+excluded.images", quarter, images)
	return err
}

func (h *ManagedHandler) serveUsageHours(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for key := range q {
		if key != "from" && key != "to" && key != "offset_minutes" {
			http.Error(w, "invalid filter", http.StatusBadRequest)
			return
		}
	}
	start, startErr := time.Parse(time.RFC3339, q.Get("from"))
	end, endErr := time.Parse(time.RFC3339, q.Get("to"))
	offset, offsetErr := strconv.Atoi(q.Get("offset_minutes"))
	if startErr != nil || endErr != nil || offsetErr != nil || !end.After(start) || end.Sub(start) > 8*24*time.Hour || offset < -840 || offset > 840 {
		http.Error(w, "invalid time range", http.StatusBadRequest)
		return
	}
	rows, err := h.db.db.Query("SELECT strftime('%Y-%m-%d',quarter_start-?*60,'unixepoch'),CAST(strftime('%H',quarter_start-?*60,'unixepoch') AS INTEGER),SUM(images),SUM(generations) FROM usage_quarters WHERE quarter_start>=? AND quarter_start<? GROUP BY 1,2 ORDER BY 1,2", offset, offset, start.Unix(), end.Unix())
	if err != nil {
		http.Error(w, "usage unavailable", http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	hours := []usageHour{}
	for rows.Next() {
		var hour usageHour
		if err := rows.Scan(&hour.Date, &hour.Hour, &hour.Count, &hour.Generations); err != nil {
			http.Error(w, "usage unavailable", http.StatusInternalServerError)
			return
		}
		hours = append(hours, hour)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "usage unavailable", http.StatusInternalServerError)
		return
	}
	jsonReply(w, http.StatusOK, map[string]any{"hours": hours})
}
