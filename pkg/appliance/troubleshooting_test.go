package appliance

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// backupRows are the quoted messages of each row of the troubleshooting table for backups onto the
// backup volume, one list per row.
func backupRows(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/troubleshooting.md")
	if err != nil {
		t.Fatal(err)
	}
	_, section, found := strings.Cut(string(raw), "### Backups onto the backup volume fail")
	if !found {
		t.Fatal("docs/troubleshooting.md has no section for backups onto the backup volume")
	}
	section, _, _ = strings.Cut(section, "\n### ")
	quoted := regexp.MustCompile(`"([^"]+)"`)
	var rows [][]string
	for line := range strings.SplitSeq(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 3 {
			continue
		}
		var row []string
		for _, m := range quoted.FindAllStringSubmatch(cells[1], -1) {
			row = append(row, m[1])
		}
		if len(row) > 0 {
			rows = append(rows, row)
		}
	}
	return rows
}

// rowMatches tells whether message reads as pattern, where … stands for any text. A pattern that
// does not open with … gives the message's start.
func rowMatches(pattern, message string) bool {
	parts := strings.Split(pattern, "…")
	if !strings.HasPrefix(message, parts[0]) {
		return false
	}
	rest := message[len(parts[0]):]
	for _, p := range parts[1:] {
		at := strings.Index(rest, p)
		if at < 0 {
			return false
		}
		rest = rest[at+len(p):]
	}
	return true
}

// troubleshootingRowsFor lists the rows of the backup table a message reads as.
func troubleshootingRowsFor(t *testing.T, message string) []string {
	t.Helper()
	var matched []string
	for _, row := range backupRows(t) {
		for _, pattern := range row {
			if rowMatches(pattern, message) {
				matched = append(matched, pattern)
				break
			}
		}
	}
	return matched
}

// Each row of the table reads as one problem: no two rows' messages could be the same message.
func TestTheBackupTroubleshootingRowsAreDistinct(t *testing.T) {
	t.Parallel()
	rows := backupRows(t)
	if len(rows) < 5 {
		t.Fatalf("read only %d rows of the backup table", len(rows))
	}
	for i, row := range rows {
		for _, pattern := range row {
			// The pattern with each … filled in is a message of its own row.
			example := strings.ReplaceAll(pattern, "…", "x")
			for j, other := range rows {
				for _, o := range other {
					if i != j && rowMatches(o, example) {
						t.Errorf("%q also reads as the row of %q", example, o)
					}
				}
			}
		}
	}
}
