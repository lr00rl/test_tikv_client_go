package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/tikv/client-go/v2/config"
	"github.com/tikv/client-go/v2/rawkv"
	"github.com/tikv/client-go/v2/txnkv"
)

func main() {
	args := os.Args[1:]

	var pdEndpoints []string
	var tableIDFilter *int64
	limit := 20
	useTxn := false
	recordType := "" // "record", "index", or "" (all)

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--table-id":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--table-id requires a value")
				os.Exit(1)
			}
			tid, err := strconv.ParseInt(args[i], 10, 64)
			if err != nil {
				fmt.Fprintln(os.Stderr, "--table-id must be a number")
				os.Exit(1)
			}
			tableIDFilter = &tid
		case "--limit":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--limit requires a value")
				os.Exit(1)
			}
			l, err := strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintln(os.Stderr, "--limit must be a number")
				os.Exit(1)
			}
			limit = l
		case "--txn":
			useTxn = true
		case "--type":
			i++
			if i >= len(args) {
				fmt.Fprintln(os.Stderr, "--type requires a value")
				os.Exit(1)
			}
			recordType = args[i]
		default:
			pdEndpoints = append(pdEndpoints, args[i])
		}
		i++
	}

	if len(pdEndpoints) == 0 {
		fmt.Fprintln(os.Stderr, "Usage: test_tikv_client_go <pd_addr> [--table-id <ID>] [--limit <N>] [--txn] [--type record|index]")
		fmt.Fprintln(os.Stderr, "Example: test_tikv_client_go 10.0.12.184:2379 --table-id 11875 --limit 5 --txn --type record")
		os.Exit(1)
	}

	mode := "RawClient"
	if useTxn {
		mode = "TransactionClient"
	}

	fmt.Printf("=== TiKV Scan Test (%s) ===\n", mode)
	fmt.Printf("PD endpoints: %v\n", pdEndpoints)
	if tableIDFilter != nil {
		fmt.Printf("Filter: table_id=%d\n", *tableIDFilter)
	}
	if recordType != "" {
		fmt.Printf("Type filter: %s\n", recordType)
	}
	fmt.Printf("Limit: %d\n\n", limit)

	// Build scan range
	var startKey, endKey []byte
	var rangeDesc string

	if tableIDFilter != nil {
		tid := *tableIDFilter
		if useTxn {
			// TransactionClient: use logical key (no memcomparable)
			startKey = append([]byte{'t'}, encodeI64(tid)...)
			switch recordType {
			case "record":
				startKey = append(startKey, "_r"...)
			case "index":
				startKey = append(startKey, "_i"...)
			}
			endKey = append([]byte{'t'}, encodeI64(tid+1)...)
		} else {
			// RawClient: use memcomparable-encoded key
			startLogical := append([]byte{'t'}, encodeI64(tid)...)
			switch recordType {
			case "record":
				startLogical = append(startLogical, "_r"...)
			case "index":
				startLogical = append(startLogical, "_i"...)
			}
			startKey = encodeMemcomparableBytes(startLogical)
			endKey = encodeTablePrefix(tid + 1)
		}
		rangeDesc = fmt.Sprintf("table_id=%d", tid)
	} else {
		startKey = []byte{'t'}
		endKey = []byte{'u'}
		rangeDesc = "all tables"
	}

	fmt.Printf("start_key (hex): %s\n", bytesToHex(startKey))
	fmt.Printf("end_key   (hex): %s\n", bytesToHex(endKey))
	fmt.Println()

	ctx := context.Background()

	if useTxn {
		fmt.Println("Connecting (TransactionClient)...")
		client, err := txnkv.NewClient(pdEndpoints)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()
		fmt.Printf("Connected!\n\n")

		txn, err := client.Begin()
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to begin txn: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Scanning %d keys for [%s]...\n", limit, rangeDesc)

		iter, err := txn.Iter(startKey, endKey)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Scan failed: %v\n", err)
			os.Exit(1)
		}
		defer iter.Close()

		var pairs []kvPair
		for iter.Valid() && len(pairs) < limit {
			k := make([]byte, len(iter.Key()))
			copy(k, iter.Key())
			v := make([]byte, len(iter.Value()))
			copy(v, iter.Value())
			pairs = append(pairs, kvPair{key: k, value: v})
			if err := iter.Next(); err != nil {
				fmt.Fprintf(os.Stderr, "Iterator error: %v\n", err)
				break
			}
		}

		fmt.Printf("Found %d key-value pairs:\n\n", len(pairs))
		for idx, kv := range pairs {
			printKV(idx, kv.key, kv.value, false)
		}

		_ = txn.Commit(ctx)
	} else {
		fmt.Println("Connecting (RawClient)...")
		client, err := rawkv.NewClient(ctx, pdEndpoints, config.DefaultConfig().Security)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to connect: %v\n", err)
			os.Exit(1)
		}
		defer client.Close()
		fmt.Printf("Connected!\n\n")

		fmt.Printf("Scanning %d keys for [%s]...\n", limit, rangeDesc)

		keys, values, err := client.Scan(ctx, startKey, endKey, limit)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Scan failed: %v\n", err)
			os.Exit(1)
		}

		fmt.Printf("Found %d key-value pairs:\n\n", len(keys))
		for idx := 0; idx < len(keys); idx++ {
			printKV(idx, keys[idx], values[idx], true)
		}
	}

	fmt.Println("=== Done ===")
}

// --- Types ---

type kvPair struct {
	key   []byte
	value []byte
}

// --- Display ---

func printKV(i int, key, value []byte, rawMode bool) {
	fmt.Printf("--- [%d] ---\n", i)
	fmt.Printf("  key (hex):  %s\n", bytesToHex(key))

	var decodedKey string
	if rawMode {
		decodedKey = decodeTiDBKeyDetailed(key)
	} else {
		decodedKey = decodeLogicalKeyDetailed(key)
	}
	fmt.Printf("  %s\n", decodedKey)

	fmt.Printf("  val (hex):  %s\n", bytesToHex(value))
	fmt.Printf("  val (len):  %d bytes\n", len(value))

	// Decode value if it's an index entry
	if len(key) > 0 && isIndexKey(key, rawMode) {
		decodeIndexValue(value)
	}

	readable := extractASCIIStrings(value, 4)
	if len(readable) > 0 {
		fmt.Printf("  val (strings): %s\n", strings.Join(readable, " | "))
	}
	fmt.Println()
}

// --- Key Classification ---

// isIndexKey determines if key is an index key (_i) vs record key (_r).
func isIndexKey(key []byte, rawMode bool) bool {
	if rawMode {
		decoded, _ := decodeMemcomparableBytes(key)
		return len(decoded) >= 11 && decoded[0] == 't' && string(decoded[9:11]) == "_i"
	}
	return len(key) >= 11 && key[0] == 't' && string(key[9:11]) == "_i"
}

// --- Logical Key Decoding (TransactionClient keys, no memcomparable layer) ---

func decodeLogicalKeyDetailed(key []byte) string {
	if len(key) == 0 || key[0] != 't' {
		return "key (tidb): not a table key"
	}
	if len(key) < 9 {
		return fmt.Sprintf("key (tidb): table key too short (%d bytes)", len(key))
	}

	tableID := decodeI64(key[1:9])

	if len(key) < 11 {
		return fmt.Sprintf("key (tidb): table_id=%d", tableID)
	}

	tag := string(key[9:11])
	switch tag {
	case "_r":
		if len(key) >= 19 {
			rowID := decodeI64(key[11:19])
			return fmt.Sprintf("key (tidb): table_id=%d, record, row_id=%d", tableID, rowID)
		}
		return fmt.Sprintf("key (tidb): table_id=%d, record (row_id truncated)", tableID)
	case "_i":
		if len(key) >= 19 {
			indexID := decodeI64(key[11:19])
			result := fmt.Sprintf("key (tidb): table_id=%d, index_id=%d", tableID, indexID)
			if len(key) > 19 {
				indexData := key[19:]
				result += "\n  key (index): " + decodeIndexColumns(indexData)
			}
			return result
		}
		return fmt.Sprintf("key (tidb): table_id=%d, index (index_id truncated)", tableID)
	default:
		return fmt.Sprintf("key (tidb): table_id=%d, unknown tag %02x%02x", tableID, key[9], key[10])
	}
}

// --- Raw Key Decoding (RawClient keys, with memcomparable layer) ---

func decodeTiDBKeyDetailed(rawKey []byte) string {
	key, consumed := decodeMemcomparableBytes(rawKey)
	remaining := len(rawKey) - consumed

	if len(key) == 0 || key[0] != 't' {
		return "key (tidb): not a table key"
	}
	if len(key) < 9 {
		return fmt.Sprintf("key (tidb): table key too short (%d decoded bytes)", len(key))
	}

	tableID := decodeI64(key[1:9])

	if len(key) < 11 {
		return fmt.Sprintf("key (tidb): table_id=%d", tableID)
	}

	tag := string(key[9:11])
	suffix := ""
	if remaining > 0 {
		suffix = fmt.Sprintf(" (+ %d bytes mvcc)", remaining)
	}

	switch tag {
	case "_r":
		if len(key) >= 19 {
			rowID := decodeI64(key[11:19])
			return fmt.Sprintf("key (tidb): table_id=%d, record, row_id=%d%s", tableID, rowID, suffix)
		}
		return fmt.Sprintf("key (tidb): table_id=%d, record (row_id truncated)%s", tableID, suffix)
	case "_i":
		if len(key) >= 19 {
			indexID := decodeI64(key[11:19])
			result := fmt.Sprintf("key (tidb): table_id=%d, index_id=%d%s", tableID, indexID, suffix)
			if len(key) > 19 {
				indexData := key[19:]
				result += "\n  key (index): " + decodeIndexColumns(indexData)
			}
			return result
		}
		return fmt.Sprintf("key (tidb): table_id=%d, index (index_id truncated)%s", tableID, suffix)
	default:
		return fmt.Sprintf("key (tidb): table_id=%d, unknown tag %02x%02x%s", tableID, key[9], key[10], suffix)
	}
}

// --- Encoding Functions ---

// encodeI64 encodes a signed int64 as big-endian with sign bit flipped (XOR 0x80).
func encodeI64(val int64) []byte {
	buf := make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(val))
	buf[0] ^= 0x80
	return buf
}

// encodeMemcomparableBytes encodes data in memcomparable format.
// Every 8 data bytes are followed by 1 marker byte:
//
//	0xff = all 8 bytes valid, more groups follow
//	0xff - N = last group, only (8-N) bytes are real data
func encodeMemcomparableBytes(data []byte) []byte {
	var encoded []byte
	pos := 0
	for {
		remaining := len(data) - pos
		if remaining >= 8 {
			encoded = append(encoded, data[pos:pos+8]...)
			encoded = append(encoded, 0xff)
			pos += 8
		} else {
			group := make([]byte, 8)
			copy(group, data[pos:])
			encoded = append(encoded, group...)
			encoded = append(encoded, 0xff-byte(8-remaining))
			break
		}
	}
	return encoded
}

// encodeTablePrefix encodes a table prefix key with memcomparable bytes encoding.
func encodeTablePrefix(tableID int64) []byte {
	logical := append([]byte{'t'}, encodeI64(tableID)...)
	return encodeMemcomparableBytes(logical)
}

// --- Decoding Functions ---

// decodeI64 decodes a sign-bit-flipped big-endian int64.
func decodeI64(data []byte) int64 {
	buf := make([]byte, 8)
	copy(buf, data[:8])
	buf[0] ^= 0x80
	return int64(binary.BigEndian.Uint64(buf))
}

// decodeMemcomparableBytes decodes memcomparable bytes encoding.
// Returns the decoded data and the number of bytes consumed from the input.
func decodeMemcomparableBytes(encoded []byte) ([]byte, int) {
	var decoded []byte
	pos := 0
	for {
		if pos+9 > len(encoded) {
			break
		}
		group := encoded[pos : pos+8]
		marker := encoded[pos+8]
		pos += 9

		if marker == 0xff {
			decoded = append(decoded, group...)
		} else {
			padCount := int(0xff - marker)
			if padCount <= 8 {
				decoded = append(decoded, group[:8-padCount]...)
			}
			break
		}
	}
	return decoded, pos
}

// decodeIndexColumns decodes index columns from the key data after table_id + _i + index_id.
func decodeIndexColumns(data []byte) string {
	var result []string
	pos := 0

	for pos < len(data) {
		typeFlag := data[pos]
		pos++

		switch typeFlag {
		case 0x03: // Int (positive): 0x03 + 8 bytes
			if pos+8 <= len(data) {
				val := decodeI64(data[pos : pos+8])
				result = append(result, fmt.Sprintf("int=%d", val))
				pos += 8
			} else {
				result = append(result, "int=<truncated>")
				goto done
			}
		case 0x01: // Bytes/String: 0x01 + data + 0xff markers
			var decoded []byte
			for {
				if pos+9 > len(data) {
					break
				}
				group := data[pos : pos+8]
				marker := data[pos+8]
				pos += 9

				if marker == 0xff {
					decoded = append(decoded, group...)
				} else {
					pad := int(0xff - marker)
					if pad <= 8 {
						decoded = append(decoded, group[:8-pad]...)
					}
					break
				}
			}
			if s := string(decoded); isValidUTF8(s) {
				result = append(result, fmt.Sprintf("str=%q", s))
			} else {
				result = append(result, fmt.Sprintf("bytes=<%d bytes>", len(decoded)))
			}
		default:
			result = append(result, fmt.Sprintf("unknown_type=0x%02x", typeFlag))
			goto done
		}
	}

done:
	if len(result) == 0 {
		return fmt.Sprintf("<%d bytes>", len(data))
	}
	return strings.Join(result, ", ")
}

// decodeIndexValue attempts to extract handle (_tidb_rowid) from index values.
func decodeIndexValue(val []byte) {
	if len(val) == 0 {
		return
	}

	if len(val) < 8 {
		return
	}

	type handleCandidate struct {
		offset int
		value  int64
	}
	var candidates []handleCandidate

	// Try position after first byte
	if len(val) >= 9 {
		h := decodeI64(val[1:9])
		if h > 0 && h < 1_000_000_000 {
			candidates = append(candidates, handleCandidate{1, h})
		}
	}

	// Try last 8 bytes
	if len(val) >= 8 {
		offset := len(val) - 8
		h := decodeI64(val[offset:])
		if h > 0 && h < 1_000_000_000 {
			candidates = append(candidates, handleCandidate{offset, h})
		}
	}

	// Try position at offset 9 (common in newer format)
	if len(val) >= 17 {
		h := decodeI64(val[9:17])
		if h > 0 && h < 1_000_000_000 {
			candidates = append(candidates, handleCandidate{9, h})
		}
	}

	if len(candidates) > 0 {
		fmt.Printf("  val (parsed): handle_candidates=%v\n", candidates)
		fmt.Printf("  val (handle): _tidb_rowid=%d (at offset %d)\n", candidates[0].value, candidates[0].offset)
	}
}

// --- Utility Functions ---

// bytesToHex converts bytes to space-separated hex string.
func bytesToHex(data []byte) string {
	parts := make([]string, len(data))
	for i, b := range data {
		parts[i] = fmt.Sprintf("%02x", b)
	}
	return strings.Join(parts, " ")
}

// extractASCIIStrings extracts printable ASCII strings (minLen or longer) from binary data.
// Skips 0xff bytes (memcomparable markers) between printable chars.
func extractASCIIStrings(data []byte, minLen int) []string {
	var result []string
	var current strings.Builder

	for _, b := range data {
		if b >= 0x20 && b < 0x7f {
			current.WriteByte(b)
		} else if b == 0xff && current.Len() > 0 {
			// memcomparable marker between printable chars - skip it
		} else {
			if current.Len() >= minLen {
				result = append(result, current.String())
			}
			current.Reset()
		}
	}
	if current.Len() >= minLen {
		result = append(result, current.String())
	}
	return result
}

// isValidUTF8 checks if a string contains only valid printable-ish characters.
func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' {
			return false
		}
	}
	return true
}
