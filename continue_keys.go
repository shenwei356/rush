package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// continueJobPrefix identifies keys whose normalized job IDs retain the input
// batch and duplicate occurrence needed to distinguish separate tasks.
const continueJobPrefix = "\x00rush-continue-job-v1:"

// continueJobOccurrences counts identical batches within one invocation.
type continueJobOccurrences map[[sha256.Size]byte]uint64

// jobContinueKey binds normalized job IDs to their input batch and occurrence.
func jobContinueKey(command string, records []string, trim string, occurrences continueJobOccurrences) string {
	if !strings.Contains(command, continueJobIDMarker) {
		return command
	}
	hash := sha256.New()
	var size [8]byte
	writePart := func(part string) {
		// Length framing distinguishes batches containing delimiters or NULs.
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(part))
	}
	writePart(command)
	for _, record := range records {
		writePart(trimInputRecord(record, trim))
	}
	var digest [sha256.Size]byte
	copy(digest[:], hash.Sum(nil))
	occurrences[digest]++
	return fmt.Sprintf("%s%x:%d\x00%s", continueJobPrefix, digest, occurrences[digest], command)
}

// continueJobCommand removes identity metadata when inspecting the expansion.
func continueJobCommand(record string) string {
	if strings.HasPrefix(record, continueJobPrefix) {
		_, command, _ := strings.Cut(record[len(continueJobPrefix):], "\x00")
		return command
	}
	return record
}

// validContinueJobKey rejects incomplete identity metadata before resume can
// mistake a damaged record for an unrelated command and repeat completed work.
func validContinueJobKey(record string) bool {
	if !strings.HasPrefix(record, continueJobPrefix) {
		return false
	}
	metadata, command, found := strings.Cut(strings.TrimPrefix(record, continueJobPrefix), "\x00")
	if !found || !strings.Contains(command, continueJobIDMarker) {
		return false
	}
	digest, ordinal, found := strings.Cut(metadata, ":")
	if !found || len(digest) != 2*sha256.Size {
		return false
	}
	decoded, err := hex.DecodeString(digest)
	if err != nil || hex.EncodeToString(decoded) != digest {
		return false
	}
	n, err := strconv.ParseUint(ordinal, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == ordinal
}
