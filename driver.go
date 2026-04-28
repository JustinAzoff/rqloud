package rqloud

// This file provides a database/sql driver that uses gorqlite with a custom
// HTTP client (tsnet). The upstream gorqlite/stdlib driver hardcodes
// gorqlite.Open() which uses a default HTTP client, causing connections to
// bypass tsnet. This driver uses gorqlite.OpenWithClient() instead.

import (
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"sync"

	"github.com/rqlite/gorqlite"
	"github.com/rqlite/gorqlite/stdlib"
)

var (
	driverMu      sync.Mutex
	driverCounter int
)

type tsnetDriver struct {
	client *http.Client
}

func (d *tsnetDriver) Open(name string) (driver.Conn, error) {
	conn, err := gorqlite.OpenWithClient(name, d.client)
	if err != nil {
		return nil, err
	}
	return &stdlib.Conn{Connection: conn}, nil
}

// registerDriver registers a new database/sql driver using the given HTTP
// client and returns the driver name. Each Server gets its own driver instance
// so multiple servers can coexist.
func registerDriver(client *http.Client) string {
	driverMu.Lock()
	defer driverMu.Unlock()
	driverCounter++
	name := fmt.Sprintf("rqlite-tsnet-%d", driverCounter)
	sql.Register(name, &tsnetDriver{client: client})
	return name
}
