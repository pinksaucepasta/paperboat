package contracttest

import (
	"bufio"
	"encoding/json"
	"os"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/doctor"
)

func TestDoctorResultVectorsMatchProductionContract(t *testing.T) {
	file, err := os.Open("../../testdata/contracts/fixtures/cli/doctor-results.ndjson")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	count := 0
	for scanner.Scan() {
		count++
		var report doctor.Report
		if err := json.Unmarshal(scanner.Bytes(), &report); err != nil {
			t.Fatalf("vector %d: %v", count, err)
		}
		if err := report.Validate(); err != nil {
			t.Fatalf("vector %d: %v", count, err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if count < 2 {
		t.Fatalf("doctor vectors=%d", count)
	}
}
