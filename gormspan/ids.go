package gormspan

import (
	"crypto/rand"
	"encoding/binary"
	"math"
	"reflect"

	"gorm.io/gorm"
)

// randomID generates a random uint64 masked to MaxInt64. This ensures
// IDs are unique across independent database replicas (CRDT-friendly)
// and safe for signed-int columns (PostgreSQL bigint, JSON numbers).
func randomID() (uint64, error) {
	var id uint64
	for id == 0 {
		if err := binary.Read(rand.Reader, binary.LittleEndian, &id); err != nil {
			return 0, err
		}
	}
	return id & math.MaxInt64, nil
}

// registerIDCallbacks adds a GORM BeforeCreate hook that assigns a
// random ID to any model whose primary key is an integer type with a
// zero value. This works generically via GORM's schema introspection
// — no upstream type modifications required.
//
// The callback is registered before gorm:begin_transaction so that the
// random ID is already set on the model when the intercept callback
// (also before gorm:begin_transaction, but registered later) serializes
// the fields for the POST payload.
func registerIDCallbacks(db *gorm.DB) {
	db.Callback().Create().Before("gorm:begin_transaction").
		Register("gormspan:randomize_id", randomizeIDCallback)
}

func randomizeIDCallback(tx *gorm.DB) {
	if tx.Statement == nil || tx.Statement.Schema == nil {
		return
	}

	for _, field := range tx.Statement.Schema.PrimaryFields {
		// Only handle integer primary keys.
		switch field.FieldType.Kind() {
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
			reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		default:
			continue
		}

		// Get the current value from the destination struct.
		val, isZero := field.ValueOf(tx.Statement.Context, tx.Statement.ReflectValue)
		if !isZero && val != nil {
			// Check if the numeric value is already non-zero.
			rv := reflect.ValueOf(val)
			if rv.Kind() >= reflect.Int && rv.Kind() <= reflect.Int64 {
				if rv.Int() != 0 {
					continue
				}
			}
			if rv.Kind() >= reflect.Uint && rv.Kind() <= reflect.Uint64 {
				if rv.Uint() != 0 {
					continue
				}
			}
		}

		id, err := randomID()
		if err != nil {
			_ = tx.AddError(err)
			return
		}

		// Set the random ID on the field, converting to the field's
		// actual type (uint, uint64, int64, NodeID, etc.).
		if err := field.Set(tx.Statement.Context, tx.Statement.ReflectValue, id); err != nil {
			_ = tx.AddError(err)
			return
		}
	}
}
