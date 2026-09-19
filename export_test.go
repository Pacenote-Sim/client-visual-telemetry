package visual

// The unexported pieces of the page, for the tests.

// LapTime is lapTime.
func LapTime(ms int) string { return lapTime(ms) }

// Address is address.
func Address(server, path string) string { return address(server, path) }
