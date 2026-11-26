package main

import "testing"

func TestReadOnlyStatementAllowed(t *testing.T) {
	tests := []struct {
		query string
		want  bool
	}{
		{"SELECT * FROM orders", true},
		{"SHOW TABLES", true},
		{"EXPLAIN UPDATE orders SET status = 'x'", false},
		{"SELECT * FROM orders FOR UPDATE", false},
		{"SET NAMES utf8mb4", true},
		{"SET GLOBAL read_only = 1", false},
		{"SET PASSWORD = 'secret'", false},
		{"START TRANSACTION READ ONLY", true},
		{"USE crm", true},
		{"SELECT * FROM orders INTO OUTFILE '/tmp/orders'", false},
		{"INSERT INTO orders (id) VALUES (?)", false},
		{"UPDATE orders SET status = 'x'", false},
		{"DELETE FROM orders", false},
		{"CREATE TABLE copy AS SELECT * FROM orders", false},
		{"SELECT 1; SHOW TABLES", true},
		{"SELECT 1; DELETE FROM orders", false},
		{"CALL refresh_orders()", false},
		{"this is not SQL", false},
	}
	for _, test := range tests {
		if got := readOnlyStatementAllowed([]byte(test.query)); got != test.want {
			t.Errorf("readOnlyStatementAllowed(%q) = %v, want %v", test.query, got, test.want)
		}
	}
}
