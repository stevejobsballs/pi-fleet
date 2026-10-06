package web

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"pi-fleet/internal/app"
	"pi-fleet/internal/domain"
	"pi-fleet/internal/fleetsync"
)

// Live fleet view (DESIGN.md §5.9). Central serves read-only JSON to
// confirmed Pis; an employee Pi renders it without storing it.

// FleetAPI is central's /v1/fleet/ handler, mounted behind the sync
// server's request-signature check.
type FleetAPI struct {
	App *app.App
}

type fleetAsset struct {
	Asset        domain.Asset
	Site         siteRow
	Location     string
	Schedules    []scheduleRow
	WorkOrders   []woRow
	Calibrations []calRow
}

type fleetCount struct {
	PartID      string `json:"part_id"`
	LocationID  string `json:"location_id"`
	ObservedQty int64  `json:"observed_qty"`
}

type fleetCountResult struct {
	Level int64 `json:"level"`
}

// Handler returns the API routes.
func (f *FleetAPI) Handler() http.Handler {
	mux := http.NewServeMux()
	s := &Server{App: f.App, Role: "central"}
	jsonOut := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			code := http.StatusInternalServerError
			if err == domain.ErrNotFound {
				code = http.StatusNotFound
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /v1/fleet/assets", func(w http.ResponseWriter, r *http.Request) {
		rows, err := listAssets(r.Context(), f.App.Store.DB(), strings.TrimSpace(r.URL.Query().Get("q")))
		jsonOut(w, rows, err)
	})
	mux.HandleFunc("GET /v1/fleet/assets/{id}", func(w http.ResponseWriter, r *http.Request) {
		d, err := s.loadAsset(r, r.PathValue("id"))
		jsonOut(w, fleetAsset{Asset: d.Asset, Site: d.Site, Location: d.Location, Schedules: d.Schedules,
			WorkOrders: d.WorkOrders, Calibrations: d.Calibrations}, err)
	})
	mux.HandleFunc("POST /v1/fleet/stock/count", func(w http.ResponseWriter, r *http.Request) {
		n, _ := fleetsync.NodeFromContext(r.Context())
		var c fleetCount
		if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
			http.Error(w, "bad JSON", http.StatusBadRequest)
			return
		}
		// Attributed to the Pi's owner; central computes the adjustment
		// against the complete ledger (DESIGN.md §5.4).
		actor := app.Actor{UserID: n.BoundUserID, SessionID: "pi:" + n.ID}
		observed := c.ObservedQty
		_, err := f.App.RecordStock(r.Context(), actor, domain.StockTxnRecorded{
			Kind: domain.StockCount, PartID: c.PartID, StockLocationID: c.LocationID, ObservedQty: &observed,
			Reason: "counted on Pi " + n.ID,
		})
		if msg := message(err); msg != "" {
			http.Error(w, msg, http.StatusConflict)
			return
		}
		if err != nil {
			jsonOut(w, nil, err)
			return
		}
		level, err := f.App.StockLevel(r.Context(), c.PartID, c.LocationID)
		jsonOut(w, fleetCountResult{Level: level}, err)
	})
	return mux
}

// FleetClient is how an employee Pi reaches central's fleet API.
type FleetClient interface {
	FleetGet(ctx context.Context, path string, out any) error
	FleetPost(ctx context.Context, path string, in, out any) error
}

func offline(err error) string {
	return "The master Pi couldn't be reached, so the whole fleet can't be searched right now (" + err.Error() + ")."
}

type fleetListData struct {
	Search string
	Assets []assetRow
	Error  string
}

func (s *Server) fleetAssets(w http.ResponseWriter, r *http.Request, sess *session) error {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	d := fleetListData{Search: q}
	if err := s.Fleet.FleetGet(r.Context(), "/v1/fleet/assets?q="+urlQuery(q), &d.Assets); err != nil {
		d.Error = offline(err)
	}
	return s.render(w, r, sess, "fleet_assets", "Whole fleet", d)
}

type fleetAssetData struct {
	Asset fleetAsset
	Error string
}

func (s *Server) fleetAsset(w http.ResponseWriter, r *http.Request, sess *session) error {
	var d fleetAssetData
	if err := s.Fleet.FleetGet(r.Context(), "/v1/fleet/assets/"+r.PathValue("id"), &d.Asset); err != nil {
		d.Error = offline(err)
	}
	title := d.Asset.Asset.Tag
	if title == "" {
		title = "Equipment"
	}
	return s.render(w, r, sess, "fleet_asset", title+" (live from the master Pi)", d)
}

// countShared records a shared stockroom count on central, where the
// complete ledger is.
func (s *Server) countShared(r *http.Request, partID, locationID string, observed int64) (string, error) {
	var res fleetCountResult
	err := s.Fleet.FleetPost(r.Context(), "/v1/fleet/stock/count", fleetCount{PartID: partID, LocationID: locationID, ObservedQty: observed}, &res)
	if err != nil {
		return "Not saved: shared stockrooms are counted on the master Pi, which couldn't be reached. Try again when online.", err
	}
	return fmt.Sprintf("Counted on the master Pi: the stock level is now %d. It will show here after the next sync.", res.Level), nil
}
