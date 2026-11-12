package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Slot struct {
	ID       string    `json:"id"`
	Provider string    `json:"provider"`
	Start    time.Time `json:"start"`
	End      time.Time `json:"end"`
}

type SlotWithAvailability struct {
	Slot
	Available bool `json:"available"`
}

type Booking struct {
	ID        string    `json:"id"`
	SlotID    string    `json:"slot_id"`
	Name      string    `json:"name"`
	Email     string    `json:"email"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

var (
	errNotFound    = errors.New("not found")
	errSlotStarted = errors.New("slot has already started")
	errSlotTaken   = errors.New("slot already booked")
	errCancelled   = errors.New("booking already cancelled")
)

const bookingCols = "id, slot_id, name, email, status, created_at"

func scanBooking(row pgx.Row) (Booking, error) {
	var b Booking
	err := row.Scan(&b.ID, &b.SlotID, &b.Name, &b.Email, &b.Status, &b.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return b, errNotFound
	}
	b.CreatedAt = b.CreatedAt.UTC()
	return b, err
}

func pgError(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return &pgconn.PgError{}
}

func isUniqueViolation(err error) bool { return pgError(err).Code == "23505" }

func newID() string {
	b := make([]byte, 8)
	rand.Read(b) // never returns an error on supported platforms
	return "bk_" + hex.EncodeToString(b)
}

// resetSlots deletes every booking and replaces all slots, atomically.
func (s *server) resetSlots(ctx context.Context, slots []Slot) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "TRUNCATE slots, bookings, idempotency_keys"); err != nil {
			return fmt.Errorf("truncate: %w", err)
		}
		_, err := tx.CopyFrom(ctx, pgx.Identifier{"slots"}, []string{"id", "provider", "start_at", "end_at"},
			pgx.CopyFromSlice(len(slots), func(i int) ([]any, error) {
				return []any{slots[i].ID, slots[i].Provider, slots[i].Start, slots[i].End}, nil
			}))
		return err
	})
}

func (s *server) listSlots(ctx context.Context, provider string) ([]SlotWithAvailability, error) {
	rows, err := s.db.Query(ctx, `
		SELECT s.id, s.provider, s.start_at, s.end_at,
		       NOT EXISTS (SELECT 1 FROM bookings b WHERE b.slot_id = s.id AND b.status = 'confirmed')
		FROM slots s
		WHERE $1 = '' OR s.provider = $1
		ORDER BY s.start_at, s.id`, provider)
	if err != nil {
		return nil, fmt.Errorf("list slots: %w", err)
	}
	slots, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (SlotWithAvailability, error) {
		var v SlotWithAvailability
		err := row.Scan(&v.ID, &v.Provider, &v.Start, &v.End, &v.Available)
		v.Start, v.End = v.Start.UTC(), v.End.UTC()
		return v, err
	})
	if slots == nil {
		slots = []SlotWithAvailability{} // JSON [] not null
	}
	return slots, err
}

// createBooking books a slot. It returns errNotFound for an unknown slot and errSlotStarted for a past one.
func (s *server) createBooking(ctx context.Context, slotID, name, email string) (Booking, error) {
	var started bool
	err := s.db.QueryRow(ctx, "SELECT start_at <= now() FROM slots WHERE id = $1", slotID).Scan(&started)
	if errors.Is(err, pgx.ErrNoRows) {
		return Booking{}, errNotFound
	}
	if err != nil {
		return Booking{}, fmt.Errorf("look up slot: %w", err)
	}
	if started {
		return Booking{}, errSlotStarted
	}
	b, err := scanBooking(s.db.QueryRow(ctx, `
		INSERT INTO bookings (id, slot_id, name, email, status)
		VALUES ($1, $2, $3, $4, 'confirmed')
		RETURNING `+bookingCols, newID(), slotID, name, email))
	switch pgErr := pgError(err); {
	case pgErr.ConstraintName == "one_confirmed_per_slot":
		// The partial unique index is the double-booking guard: of any number of
		// simultaneous inserts for one slot, Postgres lets exactly one through.
		return b, errSlotTaken
	case pgErr.Code == "23503": // slot removed by a concurrent /reset between the lookup and the insert
		return b, errNotFound
	}
	return b, err
}

// cancelBooking marks a confirmed booking cancelled, which frees its slot.
// It returns errNotFound for an unknown id and errCancelled if it was already cancelled.
func (s *server) cancelBooking(ctx context.Context, id string) (Booking, error) {
	b, err := scanBooking(s.db.QueryRow(ctx, `
		UPDATE bookings SET status = 'cancelled'
		WHERE id = $1 AND status = 'confirmed'
		RETURNING `+bookingCols, id))
	if !errors.Is(err, errNotFound) {
		return b, err
	}
	if _, err := s.getBooking(ctx, id); err != nil {
		return b, err
	}
	return b, errCancelled
}

func (s *server) getBooking(ctx context.Context, id string) (Booking, error) {
	return scanBooking(s.db.QueryRow(ctx, "SELECT "+bookingCols+" FROM bookings WHERE id = $1", id))
}
