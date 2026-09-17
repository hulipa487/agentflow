package memory

import "database/sql"

// DrainRows reads every row a query produced and returns them as a slice.
//
// It exists because a *sql.Rows is bound to the context passed to
// QueryContext: database/sql's awaitDone goroutine closes the rows as soon as
// that context is done. A driver that writes the obvious
//
//	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
//	defer cancel()
//	rows, err := db.QueryContext(ctx, ...)
//	...
//	return &rowsIter{rows: rows}, nil   // cancel fires here
//
// hands back rows that are being torn down before the caller reads them. The
// failure is intermittent and load-dependent — a small result set is usually
// buffered before the cancellation is observed, so it reads as a cold-start
// bug — and mid-iteration it surfaces as a truncated result plus a
// context.Canceled from Err(), not as an error from Query.
//
// Reading here, inside the caller's context lifetime, removes that dependency:
// a failure returns from Query, where callers already handle errors.
//
// The engine materializes the whole result set anyway
// (internal/core/caps/store.go reads every record into a slice to marshal it),
// so draining eagerly costs no extra peak memory.
func DrainRows(rows *sql.Rows, scan func(*sql.Rows) (Record, error)) ([]Record, error) {
	defer rows.Close()
	var recs []Record
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return recs, nil
}
