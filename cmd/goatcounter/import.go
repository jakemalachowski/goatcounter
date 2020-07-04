// Copyright © 2019 Martin Tournoij <martin@arp242.net>
// This file is part of GoatCounter and published under the terms of the EUPL
// v1.2, which can be found in the LICENSE file or at http://eupl12.zgo.at

package main

import (
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"time"

	"zgo.at/errors"
	"zgo.at/goatcounter"
	"zgo.at/goatcounter/cfg"
	"zgo.at/goatcounter/cron"
	"zgo.at/goatcounter/handlers"
	"zgo.at/goatcounter/pack"
	"zgo.at/zdb"
	"zgo.at/zhttp"
	"zgo.at/zlog"
)

const usageImport = `
Import pageviews from an export.

Flags:

  -db            Database connection string. Use "sqlite://<dbfile>" for SQLite,
                 or "postgres://<connect string>" for PostgreSQL
                 Default: sqlite://db/goatcounter.sqlite3

  -createdb      Create the database if it doesn't exist yet; only for SQLite.

  -debug         Modules to debug, comma-separated or 'all' for all modules.

  -site          Site to import to, not needed if there is only one site.

  -format        File format; currently accepted values:

                    csv   GoatCounter CSV export (default)

  -clear         Clear existing pageviews first.

  -replay        Replay pageviews as accurately as we can. This means that,
                 among other things, replaying an hour's worth of data will take
                 exactly one hour. This is intended for testing, debugging, and
                 benchmarking.

  -replay-start  Start the replay at 'date-month-year hour:min:sec'; everything
                 before that will be skipped.

  -replay-speed  Speed up the replay.
`

func cImport() (int, error) {
	dbConnect := flagDB()
	debug := flagDebug()

	var (
		clear, replay, createdb bool
		format, start           string
		speed                   float64
		siteID                  int64
	)
	CommandLine.Int64Var(&siteID, "site", 0, "")
	CommandLine.BoolVar(&clear, "clear", false, "")
	CommandLine.BoolVar(&createdb, "createdb", false, "")
	CommandLine.StringVar(&format, "format", "csv", "")
	CommandLine.BoolVar(&replay, "replay", false, "")
	CommandLine.Float64Var(&speed, "replay-speed", 1, "")
	CommandLine.StringVar(&start, "replay-start", "", "")
	err := CommandLine.Parse(os.Args[2:])
	if err != nil {
		return 1, err
	}

	files := CommandLine.Args()
	if len(files) == 0 {
		return 1, fmt.Errorf("need a filename")
	}
	if len(files) > 1 {
		return 1, fmt.Errorf("can only specify one filename")
	}

	var fp io.ReadCloser
	if files[0] == "-" {
		fp = ioutil.NopCloser(os.Stdin)
	} else {
		var file *os.File
		file, err = os.Open(files[0])
		if err != nil {
			return 1, err
		}
		defer file.Close()

		if strings.HasSuffix(files[0], ".gz") {
			fp, err = gzip.NewReader(file)
			if err != nil {
				return 1, err
			}
		} else {
			fp = file
		}

		defer fp.Close()
	}

	zlog.Config.SetDebug(*debug)

	db, err := connectDB(*dbConnect, nil, createdb)
	if err != nil {
		return 2, err
	}
	defer db.Close()
	ctx := zdb.With(context.Background(), db)

	// TODO: Allow by code, too.
	var site goatcounter.Site
	if siteID > 0 {
		err = site.ByID(ctx, siteID)
	} else {
		var sites goatcounter.Sites
		err = sites.List(ctx)
		if err != nil {
			return 1, err
		}

		switch len(sites) {
		case 0:
			return 1, fmt.Errorf("there are no sites in the database")
		case 1:
			site = sites[0]
		default:
			return 1, fmt.Errorf("more than one site: use -site to specify which site to import")
		}
	}
	if err != nil {
		return 1, err
	}
	ctx = goatcounter.WithSite(ctx, &site)

	if replay {
		if format != "csv" {
			return 1, errors.New("-replay can only be done with -format csv")
		}
		var s time.Time
		if start != "" {
			s, err = time.Parse("2006-01-02 15:04:05", start)
			if err != nil {
				return 1, fmt.Errorf("-start: %w", err)
			}
		}

		return importReplay(ctx, fp, speed, s)
	}

	switch format {
	default:
		return 1, fmt.Errorf("unknown -format value: %q", format)

	// TODO: this is probably the wrong way to go about it, as it may cause
	// locking issues on SQLite as two processes will be writing to the DB.
	// It would be better to send requests to a to-be-built /api/v0/count or
	// /api/v0/import.
	case "csv":
		n := 0
		cb := func() {
			hits, err := goatcounter.Memstore.Persist(ctx)
			if err != nil {
				zlog.Error(err)
				os.Exit(1)
			}

			err = cron.UpdateStats(ctx, site.ID, hits)
			if err != nil {
				zlog.Error(err)
				os.Exit(1)
			}
			n += len(hits)
			zlog.Printf("persisted %d hits", n)
		}

		goatcounter.Import(ctx, fp, clear, false, cb)
	}

	return 0, nil
}

func importReplay(ctx context.Context, fp io.Reader, speed float64, start time.Time) (int, error) {
	site := goatcounter.MustGetSite(ctx)

	// Clear all existing stats.
	err := site.DeleteAll(ctx)
	if err != nil {
		return 1, err
	}

	// Read all data in memory, grouped by second.
	c := csv.NewReader(fp)
	header, err := c.Read()
	if err != nil {
		return 1, err
	}

	if len(header) == 0 || !strings.HasPrefix(header[0], goatcounter.ExportVersion) {
		return 1, errors.Errorf(
			"wrong version of CSV database: %s (expected: %s)",
			header[0][:1], goatcounter.ExportVersion)
	}

	nextIP := int32(0)
	ips := make(map[string]string)
	requests := make(map[int64][]*http.Request)
	for {
		row, err := c.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return 1, err
		}
		if len(row) != 12 {
			return 1, fmt.Errorf("wrong number of fields: %d (want: 12)", len(row))
		}

		path, title, event, bot, session, firstVisit, ref, refScheme, browser,
			size, location, createdAt := row[0], row[1], row[2], row[3], row[4],
			row[5], row[6], row[7], row[8], row[9], row[10], row[11]

		created, _ := time.Parse(time.RFC3339, createdAt)
		if start.IsZero() { // Assume it's sorted by date.
			start = created
		}
		q := make(url.Values)
		q.Set("p", path)
		q.Set("t", title)
		q.Set("e", event)
		q.Set("b", bot)
		q.Set("r", ref)
		q.Set("s", size)

		_ = firstVisit
		_ = refScheme
		_ = location

		r, _ := http.NewRequest("GET", site.URL()+"/count?"+q.Encode(), nil)
		r.Header.Set("User-Agent", browser)
		r.Header.Set("Content-Type", "application/json")

		ip, ok := ips[session]
		if !ok {
			ip = fmt.Sprintf("%d.%d.%d.%d", nextIP>>24, nextIP>>16%256, nextIP>>8%256, nextIP%256)
			ips[session] = ip
			nextIP++
		}
		r.RemoteAddr = ip

		requests[created.Unix()] = append(requests[created.Unix()], r)
	}

	// TODO: print distribution overview.

	zhttp.InitTpl(pack.Templates)
	cfg.Serve = true
	handler := handlers.NewBackend(zdb.MustGet(ctx), nil)

	sleep := 1 * time.Second
	if speed > 1 {
		sleep = time.Duration(1_000_000_000 / speed)
	}

	now := start
	goatcounter.Now = func() time.Time { return now }
	for {
		reqs := requests[now.Unix()]
		delete(requests, now.Unix())
		if len(reqs) > 0 {
			fmt.Printf("%d requests for %s\n", len(reqs), now.Format("2006-01-02 15:04:05"))
		}

		go func(reqs []*http.Request) {
			for _, r := range reqs {
				rr := httptest.NewRecorder()
				handler.ServeHTTP(rr, r)
				if rr.Code != 200 {
					fmt.Printf("status %d: %s\n", rr.Code, rr.Header().Get("X-Goatcounter"))
				}
			}
		}(reqs)

		now = now.Add(1 * time.Second)
		if now.After(time.Now()) {
			break
		}

		time.Sleep(sleep)
	}

	return 0, nil
}
