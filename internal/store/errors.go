package store

import "errors"

// ErrDatastoreUnavailable is returned when the connection pool has not yet
// reached the ready state or the Postgres socket is unreachable.
var ErrDatastoreUnavailable = errors.New("store: datastore unavailable")

// ErrSchemaVersion is returned by AssertCompatible when the live schema
// version does not match the binary's expected version.
var ErrSchemaVersion = errors.New("store: schema version mismatch")
