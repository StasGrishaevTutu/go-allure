package gotest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/robotomize/go-allure/internal/slice"
)

type NestedTest struct {
	Value    Test
	Children []NestedTest
	Log      []byte
}

type Set struct {
	Err   error
	Tests []NestedTest
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewScanner(r)}
}

type Reader struct {
	r *bufio.Scanner
}

// ReadAll function on the Reader struct that takes in a context.Context and returns a Set and an error.
func (r *Reader) ReadAll(ctx context.Context) (Set, error) {
	var errs []error

	prefix := &prefixNode{}

	// Iterate through each line in the scanner.
	// If the context is done, return an empty Set and the context error.
	// Parse the line as a JSON object and update the corresponding Test object in the prefix tree.
	for r.r.Scan() {
		select {
		case <-ctx.Done():
			return Set{}, ctx.Err()
		default:
		}

		line := r.r.Bytes()

		var row Entry
		if err := json.Unmarshal(line, &row); err != nil {
			errs = append(errs, fmt.Errorf("json.Unmarshal: %w", err))
		}

		if len(row.TestName) > 0 {
			key := row.Package + "/" + row.TestName

			tc, ok := prefix.find(key)
			if !ok {
				obj := &Test{
					Name:    row.TestName,
					Package: row.Package,
				}
				prefix.insert(obj)
				tc = obj
			}

			tc.Update(row)
		}
	}

	result := Set{
		Err: errors.Join(errs...),
	}

	// Create a slice to hold NestedTest instances.
	// Iterate through each child in the prefix tree.
	// If the walk function returns a NestedTest and true, append the NestedTest to the testCases slice.
	testCases := make([]NestedTest, 0, len(prefix.Children))
	for _, nod := range prefix.Children {
		if tc, ok := r.walk(nod, newPrefixLog()); ok {
			testCases = append(testCases, tc)
		}
	}

	result.Tests = make([]NestedTest, len(testCases))
	copy(result.Tests, testCases)

	return result, nil
}

// The walk function takes in a prefix node and a prefix log as parameters
// and returns a NestedTest struct and a boolean value.

func (r *Reader) walk(node *prefixNode, prefix *prefixLog) (NestedTest, bool) {
	var testCase NestedTest

	if node == nil {
		return testCase, false
	}

	testCase.Value = *node.Value

	// Define an isResultActionRow function that checks if the given string is a
	// result action row (contains "---" and either "FAIL", "PASS", or "SKIP").
	isResultActionRow := func(s string) bool {
		isGroupPrefix := strings.Contains(s, "---")
		isAction := strings.Contains(s, strings.ToUpper(ActionFail)) ||
			strings.Contains(s, strings.ToUpper(ActionPass)) ||
			strings.Contains(s, strings.ToUpper(ActionSkip))

		return isGroupPrefix && isAction
	}

	// Iterate through each output in the testCase value's output field.
	// If an output is a result action row, add the prefix to it and append it to the prefix buffer.
	output := testCase.Value.Output
	for idx := range output {
		if isResultActionRow(output[idx]) {
			output[idx] = prefix.prefix + output[idx]
		}
		prefix.buf.WriteString(output[idx])
	}

	// Clear the output field of the testCase value.
	testCase.Value.Output = testCase.Value.Output[:0]

	// Increment the prefix and defer decrementing it to ensure it always gets decremented.
	prefix.incrPrefix()
	defer prefix.decrPrefix()

	// Iterate through each child in the node's children field.
	// If the recursive walk function returns a testCase and true,
	// append the returned testCase to the current testCase's children field.
	for _, nod := range node.Children {
		if child, ok := r.walk(nod, prefix.copy()); ok {
			testCase.Children = append(testCase.Children, child)
		}
	}

	// Create a new reader with the prefix buffer's bytes.
	// Seek the reader to the current prefix position in the buffer.
	reader := bytes.NewReader(prefix.buf.Bytes())
	if _, err := reader.Seek(int64(prefix.pos), io.SeekCurrent); err != nil {
		return NestedTest{}, false
	}

	// Read all the bytes from the reader and convert it into a string slice.
	all, err := io.ReadAll(reader)
	if err != nil {
		return NestedTest{}, false
	}

	// Convert bytes to strings
	stringsSlice := slice.Map(
		bytes.Split(all, []byte{'\n'}), func(t []byte) string {
			return string(t) + "\n"
		},
	)

	// Remove the newline character from the last string in the slice.
	lastIdx := len(stringsSlice) - 1
	stringsSlice[lastIdx] = strings.TrimSuffix(stringsSlice[lastIdx], "\n")

	// Define a mark slice to hold result action rows.
	// Initialize mx to the maximum possible integer value.
	mark := make([]string, 0)
	mx := 1<<31 - 1

	// Iterate through the string slice from the end to the beginning.
	// If a string is a result action row, append it to the mark slice
	// and remove it from the string slice.
	// Update mx to the minimum indentation count found among result action rows.
	for i := len(stringsSlice) - 1; i >= 0; i-- {
		if isResultActionRow(stringsSlice[i]) {
			cnt := strings.Count(stringsSlice[i], whitespaceIndent)
			if cnt < mx {
				mx = cnt
			}

			mark = append(mark, stringsSlice[i])
			stringsSlice = append(stringsSlice[:i], stringsSlice[i+1:]...)
		}
	}

	// Sort the mark slice by the number of whitespace indents.
	sort.Slice(
		mark, func(i, j int) bool {
			return strings.Count(mark[i], whitespaceIndent) < strings.Count(mark[j], whitespaceIndent)
		},
	)

	// Remove the whitespace for all rows in the stringsSlice and mark slices,
	// according to the minimum indentation count (mx), and concatenate them into a byte slice.
	log := make([]byte, 0, len(stringsSlice)+len(mark))
	for _, row := range append(stringsSlice, mark...) {
		log = append(log, []byte(strings.Replace(row, whitespaceIndent, "", mx))...)
	}

	testCase.Log = log

	return testCase, true
}

func newPrefixLog() *prefixLog {
	return &prefixLog{buf: bytes.NewBuffer(make([]byte, 0, 64))}
}

const whitespaceIndent = "    "

type prefixLog struct {
	prefix string
	buf    *bytes.Buffer
	pos    int
}

func (r *prefixLog) copy() *prefixLog {
	r1 := newPrefixLog()
	r1.buf = r.buf
	r1.prefix = r.prefix
	r1.pos = r.buf.Len()
	return r1
}

func (r *prefixLog) incrPrefix() {
	r.prefix += whitespaceIndent
}

func (r *prefixLog) decrPrefix() {
	r.prefix = strings.TrimSuffix(r.prefix, whitespaceIndent)
}
