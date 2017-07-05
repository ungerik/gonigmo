package gonigmo

/*
#include <stdlib.h>
#include <onigmo.h>
#include "chelper.h"
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"sync"
	"unicode/utf8"
	"unsafe"
)

type strRange []int

const (
	numMatchStartSize      = 4
	numReadBufferStartSize = 256
)

var mutex sync.Mutex

type matchData struct {
	count   int
	indices [][]int32
}

type namedGroupInfo map[string]int

// Regexp is the representation of a compiled regular expression.
// A Regexp is safe for concurrent use by multiple goroutines.
type Regexp struct {
	pattern        string
	regex          C.OnigRegex
	region         *C.OnigRegion
	encoding       C.OnigEncoding
	errorInfo      *C.OnigErrorInfo
	errorBuf       *C.char
	matchData      *matchData
	namedGroupInfo namedGroupInfo
	mutex          *sync.Mutex
}

// NewRegexp creates a new Regexp with given pattern and options.
func NewRegexp(pattern string, option int) (re *Regexp, err error) {
	re = &Regexp{pattern: pattern}
	patternCharPtr := C.CString(pattern)
	defer C.free(unsafe.Pointer(patternCharPtr))

	mutex.Lock()
	defer mutex.Unlock()
	errCode := C.NewOnigRegex(patternCharPtr, C.int(len(pattern)), C.int(option), &re.regex, &re.region, &re.encoding, &re.errorInfo, &re.errorBuf)
	if errCode != C.ONIG_NORMAL {
		err = errors.New(C.GoString(re.errorBuf))
	} else {
		err = nil
		numCapturesInPattern := int(C.onig_number_of_captures(re.regex)) + 1
		re.matchData = new(matchData)
		re.matchData.indices = make([][]int32, numMatchStartSize)
		for i := 0; i < numMatchStartSize; i++ {
			re.matchData.indices[i] = make([]int32, numCapturesInPattern*2)
		}
		re.namedGroupInfo = re.getNamedGroupInfo()
		//runtime.SetFinalizer(re, (*Regexp).Free)
	}
	return re, err
}

// Compile parses a regular expression and returns, if successful,
// a Regexp object that can be used to match against text.
func Compile(str string) (*Regexp, error) {
	return NewRegexp(str, ONIG_OPTION_DEFAULT)
}

// MustCompile is like Compile but panics if the expression cannot be parsed.
// It simplifies safe initialization of global variables holding compiled regular
// expressions.
func MustCompile(str string) *Regexp {
	regexp, error := NewRegexp(str, ONIG_OPTION_DEFAULT)
	if error != nil {
		panic("regexp: compiling " + str + ": " + error.Error())
	}
	return regexp
}

func MustCompileThreadsafe(str string) *Regexp {
	return MustCompile(str).MakeThreadsafe()
}

// CompileWithOption parses a regular expression and returns, if successful,
// a Regexp object that can be used to match against text.
func CompileWithOption(str string, option int) (*Regexp, error) {
	return NewRegexp(str, option)
}

// MustCompileWithOption is like Compile but panics if the expression cannot be parsed.
// It simplifies safe initialization of global variables holding compiled regular
// expressions.
func MustCompileWithOption(str string, option int) *Regexp {
	regexp, error := NewRegexp(str, option)
	if error != nil {
		panic("regexp: compiling " + str + ": " + error.Error())
	}
	return regexp
}

func (re *Regexp) Pattern() string {
	return re.pattern
}

func (re *Regexp) MakeThreadsafe() *Regexp {
	re.mutex = new(sync.Mutex)
	return re
}

func (re *Regexp) lock() {
	if re.mutex != nil {
		re.mutex.Lock()
	}
}
func (re *Regexp) unlock() {
	if re.mutex != nil {
		re.mutex.Unlock()
	}
}

func (re *Regexp) unlockCopy(indices []int) (result []int) {
	if re.mutex == nil {
		return indices
	}
	if indices != nil {
		result = make([]int, len(indices))
		copy(result, indices)
	}
	re.mutex.Unlock()
	return result
}

func (re *Regexp) unlockCopy2(indices [][]int) (result [][]int) {
	if re.mutex == nil {
		return indices
	}
	if indices != nil {
		result = make([][]int, len(indices))
		for i := range result {
			result[i] = make([]int, len(indices[i]))
			copy(result[i], indices[i])
		}
	}
	re.mutex.Unlock()
	return result
}

// Free frees underlying C memory
func (re *Regexp) Free() {
	re.lock()
	defer re.unlock()

	mutex.Lock()
	if re.regex != nil {
		C.onig_free(re.regex)
		re.regex = nil
	}
	if re.region != nil {
		C.onig_region_free(re.region, 1)
		re.region = nil
	}
	mutex.Unlock()
	if re.errorInfo != nil {
		C.free(unsafe.Pointer(re.errorInfo))
		re.errorInfo = nil
	}
	if re.errorBuf != nil {
		C.free(unsafe.Pointer(re.errorBuf))
		re.errorBuf = nil
	}
}

func (re *Regexp) getNamedGroupInfo() (namedGroupInfo namedGroupInfo) {
	numNamedGroups := int(C.onig_number_of_names(re.regex))
	//when any named capture exisits, there is no numbered capture even if there are unnamed captures
	if numNamedGroups > 0 {
		namedGroupInfo = make(map[string]int)
		//try to get the names
		bufferSize := len(re.pattern) * 2
		nameBuffer := make([]byte, bufferSize)
		groupNumbers := make([]int32, numNamedGroups)
		bufferPtr := unsafe.Pointer(&nameBuffer[0])
		numbersPtr := unsafe.Pointer(&groupNumbers[0])
		length := int(C.GetCaptureNames(re.regex, bufferPtr, (C.int)(bufferSize), (*C.int)(numbersPtr)))
		if length > 0 {
			namesAsBytes := bytes.Split(nameBuffer[:length], []byte(";"))
			if len(namesAsBytes) != numNamedGroups {
				log.Fatalf("the number of named groups (%d) does not match the number names found (%d)\n", numNamedGroups, len(namesAsBytes))
			}
			for i, nameAsBytes := range namesAsBytes {
				name := string(nameAsBytes)
				namedGroupInfo[name] = int(groupNumbers[i])
			}
		} else {
			log.Fatalf("could not get the capture group names from %q", re.String())
		}
	}
	return
}

func (re *Regexp) groupNameToID(name string) (id int) {
	if re.namedGroupInfo == nil {
		id = ONIGERR_UNDEFINED_NAME_REFERENCE
	} else {
		id = re.namedGroupInfo[name]
	}
	return
}

func (re *Regexp) processMatch(numCaptures int) (match []int32) {
	if numCaptures <= 0 {
		panic("cannot have 0 captures when processing a match")
	}
	matchData := re.matchData
	return matchData.indices[matchData.count][:numCaptures*2]
}

// clearMatchData clears the last match data
func (re *Regexp) clearMatchData() {
	re.matchData.count = 0
}

func (re *Regexp) find(b []byte, n int, offset int) (match []int) {
	if n == 0 {
		b = []byte{0}
	}
	ptr := unsafe.Pointer(&b[0])
	matchData := re.matchData
	capturesPtr := unsafe.Pointer(&matchData.indices[matchData.count][0])
	numCaptures := int32(0)
	numCapturesPtr := unsafe.Pointer(&numCaptures)
	pos := C.SearchOnigRegex(
		ptr,
		C.int(n),
		C.int(offset),
		C.int(ONIG_OPTION_DEFAULT),
		re.regex,
		re.region,
		re.errorInfo,
		(*C.char)(nil),
		(*C.int)(capturesPtr),
		(*C.int)(numCapturesPtr),
	)
	if pos >= 0 {
		if numCaptures <= 0 {
			panic("cannot have 0 captures when processing a match")
		}
		match2 := matchData.indices[matchData.count][:numCaptures*2]
		match = make([]int, len(match2))
		for i := range match2 {
			match[i] = int(match2[i])
		}
		numCapturesInPattern := int32(C.onig_number_of_captures(re.regex)) + 1
		if numCapturesInPattern != numCaptures {
			log.Fatalf("expected %d captures but got %d\n", numCapturesInPattern, numCaptures)
		}
	}
	return match
}

func extract(b []byte, beg int, end int) []byte {
	if beg < 0 || end < 0 {
		return nil
	}
	return b[beg:end]
}

func (re *Regexp) match(b []byte, n int, offset int) bool {
	re.clearMatchData()
	if n == 0 {
		b = []byte{0}
	}
	ptr := unsafe.Pointer(&b[0])
	pos := int(C.SearchOnigRegex(ptr, C.int(n), C.int(offset), C.int(ONIG_OPTION_DEFAULT), re.regex, re.region, re.errorInfo, (*C.char)(nil), (*C.int)(nil), (*C.int)(nil)))
	return pos >= 0
}

func (re *Regexp) findAll(b []byte, n int) (matches [][]int) {
	re.clearMatchData()

	if n < 0 {
		n = len(b)
	}
	matchData := re.matchData
	offset := 0
	for offset <= n {
		if matchData.count >= len(matchData.indices) {
			length := len(matchData.indices[0])
			matchData.indices = append(matchData.indices, make([]int32, length))
		}
		if match := re.find(b, n, offset); len(match) > 0 {
			matchData.count++
			//move offset to the ending index of the current match and prepare to find the next non-overlapping match
			offset = match[1]
			//if match[0] == match[1], it means the current match does not advance the search. we need to exit the loop to avoid getting stuck here.
			if match[0] == match[1] {
				if offset < n && offset >= 0 {
					//there are more bytes, so move offset by a word
					_, width := utf8.DecodeRune(b[offset:])
					offset += width
				} else {
					//search is over, exit loop
					break
				}
			}
		} else {
			break
		}
	}
	matches2 := matchData.indices[:matchData.count]
	matches = make([][]int, len(matches2))
	for i, v := range matches2 {
		matches[i] = make([]int, len(v))
		for j, v2 := range v {
			matches[i][j] = int(v2)
		}
	}
	return matches
}

// Find returns a slice holding the text of the leftmost match in b of the regular expression.
// A return value of nil indicates no match.
func (re *Regexp) Find(b []byte) []byte {
	loc := re.FindIndex(b)
	if loc == nil {
		return nil
	}
	return extract(b, loc[0], loc[1])
}

// FindIndex returns a two-element slice of integers defining the location of
// the leftmost match in b of the regular expression. The match itself is at
// b[loc[0]:loc[1]].
// A return value of nil indicates no match.
func (re *Regexp) FindIndex(b []byte) []int {
	re.lock()
	re.clearMatchData()
	match := re.find(b, len(b), 0)
	if len(match) == 0 {
		re.unlock()
		return nil
	}
	return re.unlockCopy(match[:2])
}

// FindString returns a string holding the text of the leftmost match in s of the regular
// expression. If there is no match, the return value is an empty string,
// but it will also be empty if the regular expression successfully matches
// an empty string. Use FindStringIndex or FindStringSubmatch if it is
// necessary to distinguish these cases.
func (re *Regexp) FindString(s string) string {
	b := []byte(s)
	mb := re.Find(b)
	if mb == nil {
		return ""
	}
	return string(mb)
}

// FindStringIndex returns a two-element slice of integers defining the
// location of the leftmost match in s of the regular expression. The match
// itself is at s[loc[0]:loc[1]].
// A return value of nil indicates no match.
func (re *Regexp) FindStringIndex(s string) []int {
	return re.FindIndex([]byte(s))
}

// FindAllIndex is the 'All' version of FindIndex; it returns a slice of all
// successive matches of the expression, as defined by the 'All' description
// in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllIndex(b []byte, n int) [][]int {
	re.lock()
	matches := re.findAll(b, n)
	if len(matches) == 0 {
		re.unlock()
		return nil
	}
	return re.unlockCopy2(matches)
}

// FindAll is the 'All' version of Find; it returns a slice of all successive
// matches of the expression, as defined by the 'All' description in the
// package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAll(b []byte, n int) [][]byte {
	matches := re.FindAllIndex(b, n)
	if matches == nil {
		return nil
	}
	matchBytes := make([][]byte, 0, len(matches))
	for _, match := range matches {
		matchBytes = append(matchBytes, extract(b, match[0], match[1]))
	}
	return matchBytes
}

// FindAllString is the 'All' version of FindString; it returns a slice of all
// successive matches of the expression, as defined by the 'All' description
// in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllString(s string, n int) []string {
	b := []byte(s)
	matches := re.FindAllIndex(b, n)
	if matches == nil {
		return nil
	}
	matchStrings := make([]string, 0, len(matches))
	for _, match := range matches {
		m := extract(b, match[0], match[1])
		if m == nil {
			matchStrings = append(matchStrings, "")
		} else {
			matchStrings = append(matchStrings, string(m))
		}
	}
	return matchStrings

}

// FindAllStringIndex is the 'All' version of FindStringIndex; it returns a
// slice of all successive matches of the expression, as defined by the 'All'
// description in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllStringIndex(s string, n int) [][]int {
	return re.FindAllIndex([]byte(s), n)
}

func (re *Regexp) findSubmatchIndex(b []byte) []int {
	re.clearMatchData()
	return re.find(b, len(b), 0)
}

// FindSubmatchIndex returns a slice holding the index pairs identifying the
// leftmost match of the regular expression in b and the matches, if any, of
// its subexpressions, as defined by the 'Submatch' and 'Index' descriptions
// in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindSubmatchIndex(b []byte) []int {
	re.lock()
	match := re.findSubmatchIndex(b)
	if len(match) == 0 {
		re.unlock()
		return nil
	}
	return re.unlockCopy(match)
}

// FindSubmatch returns a slice of slices holding the text of the leftmost
// match of the regular expression in b and the matches, if any, of its
// subexpressions, as defined by the 'Submatch' descriptions in the package
// comment.
// A return value of nil indicates no match.
func (re *Regexp) FindSubmatch(b []byte) [][]byte {
	match := re.FindSubmatchIndex(b)
	if match == nil {
		return nil
	}
	length := len(match) / 2
	if length == 0 {
		return nil
	}
	results := make([][]byte, 0, length)
	for i := 0; i < length; i++ {
		results = append(results, extract(b, match[2*i], match[2*i+1]))
	}
	return results
}

// FindStringSubmatch returns a slice of strings holding the text of the
// leftmost match of the regular expression in s and the matches, if any, of
// its subexpressions, as defined by the 'Submatch' description in the
// package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindStringSubmatch(s string) []string {
	b := []byte(s)
	match := re.FindSubmatchIndex(b)
	if match == nil {
		return nil
	}
	length := len(match) / 2
	if length == 0 {
		return nil
	}

	results := make([]string, 0, length)
	for i := 0; i < length; i++ {
		cap := extract(b, match[2*i], match[2*i+1])
		if cap == nil {
			results = append(results, "")
		} else {
			results = append(results, string(cap))
		}
	}
	return results
}

// FindStringSubmatchIndex returns a slice holding the index pairs
// identifying the leftmost match of the regular expression in s and the
// matches, if any, of its subexpressions, as defined by the 'Submatch' and
// 'Index' descriptions in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindStringSubmatchIndex(s string) []int {
	return re.FindSubmatchIndex([]byte(s))
}

// FindAllSubmatchIndex is the 'All' version of FindSubmatchIndex; it returns
// a slice of all successive matches of the expression, as defined by the
// 'All' description in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllSubmatchIndex(b []byte, n int) [][]int {
	re.lock()
	matches := re.findAll(b, n)
	if len(matches) == 0 {
		re.unlock()
		return nil
	}
	return re.unlockCopy2(matches)
}

// FindAllSubmatch is the 'All' version of FindSubmatch; it returns a slice
// of all successive matches of the expression, as defined by the 'All'
// description in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllSubmatch(b []byte, n int) [][][]byte {
	matches := re.FindAllIndex(b, n)
	if len(matches) == 0 {
		return nil
	}
	allCapturedBytes := make([][][]byte, 0, len(matches))
	for _, match := range matches {
		length := len(match) / 2
		capturedBytes := make([][]byte, 0, length)
		for i := 0; i < length; i++ {
			capturedBytes = append(capturedBytes, extract(b, match[2*i], match[2*i+1]))
		}
		allCapturedBytes = append(allCapturedBytes, capturedBytes)
	}

	return allCapturedBytes
}

// FindAllStringSubmatch is the 'All' version of FindStringSubmatch; it
// returns a slice of all successive matches of the expression, as defined by
// the 'All' description in the package comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllStringSubmatch(s string, n int) [][]string {
	b := []byte(s)
	matches := re.FindAllIndex(b, n)
	if len(matches) == 0 {
		return nil
	}
	allCapturedStrings := make([][]string, 0, len(matches))
	for _, match := range matches {
		length := len(match) / 2
		capturedStrings := make([]string, 0, length)
		for i := 0; i < length; i++ {
			cap := extract(b, match[2*i], match[2*i+1])
			if cap == nil {
				capturedStrings = append(capturedStrings, "")
			} else {
				capturedStrings = append(capturedStrings, string(cap))
			}
		}
		allCapturedStrings = append(allCapturedStrings, capturedStrings)
	}
	return allCapturedStrings
}

// FindAllStringSubmatchIndex is the 'All' version of
// FindStringSubmatchIndex; it returns a slice of all successive matches of
// the expression, as defined by the 'All' description in the package
// comment.
// A return value of nil indicates no match.
func (re *Regexp) FindAllStringSubmatchIndex(s string, n int) [][]int {
	return re.FindAllSubmatchIndex([]byte(s), n)
}

// Match checks whether a textual regular expression
// matches a byte slice. More complicated queries need
// to use Compile and the full Regexp interface.
func Match(pattern string, b []byte) (matched bool, err error) {
	re, err := Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.Match(b), nil
}

// MatchReader checks whether a textual regular expression matches the text
// read by the RuneReader. More complicated queries need to use Compile and
// the full Regexp interface.
func MatchReader(pattern string, r io.RuneReader) (matched bool, err error) {
	re, err := Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchReader(r), nil
}

// Match reports whether the Regexp matches the byte slice b.
func (re *Regexp) Match(b []byte) bool {
	re.lock()
	result := re.match(b, len(b), 0)
	re.unlock()
	return result
}

// MatchString reports whether the Regexp matches the string s.
func (re *Regexp) MatchString(s string) bool {
	return re.Match([]byte(s))
}

// NumSubexp returns the number of parenthesized subexpressions in this Regexp.
func (re *Regexp) NumSubexp() int {
	re.lock()
	result := (int)(C.onig_number_of_captures(re.regex))
	re.unlock()
	return result
}

func (re *Regexp) getNamedCapture(name []byte, capturedBytes [][]byte) []byte {
	nameStr := string(name)
	capNum := re.groupNameToID(nameStr)
	if capNum < 0 || capNum >= len(capturedBytes) {
		panic(fmt.Sprintf("capture group name (%q) has error\n", nameStr))
	}
	return capturedBytes[capNum]
}

func (re *Regexp) getNumberedCapture(num int, capturedBytes [][]byte) []byte {
	//when named capture groups exist, numbered capture groups returns ""
	if re.namedGroupInfo == nil && num <= (len(capturedBytes)-1) && num >= 0 {
		return capturedBytes[num]
	}
	return []byte("")
}

func fillCapturedValues(repl []byte, _ []byte, capturedBytes map[string][]byte) []byte {
	replLen := len(repl)
	newRepl := make([]byte, 0, replLen*3)
	inEscapeMode := false
	inGroupNameMode := false
	groupName := make([]byte, 0, replLen)
	for index := 0; index < replLen; index++ {
		ch := repl[index]
		if inGroupNameMode && ch == byte('<') {
		} else if inGroupNameMode && ch == byte('>') {
			inGroupNameMode = false
			groupNameStr := string(groupName)
			capBytes := capturedBytes[groupNameStr]
			newRepl = append(newRepl, capBytes...)
			groupName = groupName[:0] //reset the name
		} else if inGroupNameMode {
			groupName = append(groupName, ch)
		} else if inEscapeMode && ch <= byte('9') && byte('1') <= ch {
			capNumStr := string(ch)
			capBytes := capturedBytes[capNumStr]
			newRepl = append(newRepl, capBytes...)
		} else if inEscapeMode && ch == byte('k') && (index+1) < replLen && repl[index+1] == byte('<') {
			inGroupNameMode = true
			inEscapeMode = false
			index++ //bypass the next char '<'
		} else if inEscapeMode {
			newRepl = append(newRepl, '\\')
			newRepl = append(newRepl, ch)
		} else if ch != '\\' {
			newRepl = append(newRepl, ch)
		}
		if ch == byte('\\') || inEscapeMode {
			inEscapeMode = !inEscapeMode
		}
	}
	return newRepl
}

func (re *Regexp) replaceAll(src, repl []byte, replFunc func([]byte, []byte, map[string][]byte) []byte) []byte {
	srcLen := len(src)
	matches := re.findAll(src, srcLen)
	if len(matches) == 0 {
		return src
	}
	dest := make([]byte, 0, srcLen)
	for i, match := range matches {
		length := len(match) / 2
		capturedBytes := make(map[string][]byte)
		if re.namedGroupInfo == nil {
			for j := 0; j < length; j++ {
				capturedBytes[strconv.Itoa(j)] = extract(src, match[2*j], match[2*j+1])
			}
		} else {
			for name, j := range re.namedGroupInfo {
				capturedBytes[name] = extract(src, match[2*j], match[2*j+1])
			}
		}
		matchBytes := extract(src, match[0], match[1])
		newRepl := replFunc(repl, matchBytes, capturedBytes)
		prevEnd := 0
		if i > 0 {
			prevMatch := matches[i-1][:2]
			prevEnd = prevMatch[1]
		}
		if match[0] > prevEnd && prevEnd >= 0 && match[0] <= srcLen {
			dest = append(dest, src[prevEnd:match[0]]...)
		}
		dest = append(dest, newRepl...)
	}
	lastEnd := matches[len(matches)-1][1]
	if lastEnd < srcLen && lastEnd >= 0 {
		dest = append(dest, src[lastEnd:]...)
	}
	return dest
}

// ReplaceAll returns a copy of src, replacing matches of the Regexp
// with the replacement text repl. Inside repl, $ signs are interpreted as
// in Expand, so for instance $1 represents the text of the first submatch.
func (re *Regexp) ReplaceAll(src, repl []byte) []byte {
	re.lock()
	result := re.replaceAll(src, repl, fillCapturedValues)
	re.unlock()
	return result
}

// ReplaceAllFunc returns a copy of src in which all matches of the
// Regexp have been replaced by the return value of function repl applied
// to the matched byte slice. The replacement returned by repl is substituted
// directly, without using Expand.
func (re *Regexp) ReplaceAllFunc(src []byte, repl func([]byte) []byte) []byte {
	re.lock()
	result := re.replaceAll(src, []byte(""), func(_ []byte, matchBytes []byte, _ map[string][]byte) []byte {
		return repl(matchBytes)
	})
	re.unlock()
	return result
}

// ReplaceAllString returns a copy of src, replacing matches of the Regexp
// with the replacement string repl. Inside repl, $ signs are interpreted as
// in Expand, so for instance $1 represents the text of the first submatch.
func (re *Regexp) ReplaceAllString(src, repl string) string {
	return string(re.ReplaceAll([]byte(src), []byte(repl)))
}

// ReplaceAllStringFunc returns a copy of src in which all matches of the
// Regexp have been replaced by the return value of function repl applied
// to the matched substring. The replacement returned by repl is substituted
// directly, without using Expand.
func (re *Regexp) ReplaceAllStringFunc(src string, repl func(string) string) string {
	srcB := []byte(src)
	re.lock()
	destB := re.replaceAll(srcB, []byte(""), func(_ []byte, matchBytes []byte, _ map[string][]byte) []byte {
		return []byte(repl(string(matchBytes)))
	})
	re.unlock()
	return string(destB)
}

// String returns the source text used to compile the regular expression.
func (re *Regexp) String() string {
	re.lock()
	result := re.pattern
	re.unlock()
	return result
}

func growBuffer(b []byte, offset int, n int) []byte {
	if offset+n > cap(b) {
		buf := make([]byte, 2*cap(b)+n)
		copy(buf, b[:offset])
		return buf
	}
	return b
}

func fromReader(r io.RuneReader) []byte {
	b := make([]byte, numReadBufferStartSize)
	offset := 0
	var err error
	for err == nil {
		rune, runeWidth, err := r.ReadRune()
		if err == nil {
			b = growBuffer(b, offset, runeWidth)
			writeWidth := utf8.EncodeRune(b[offset:], rune)
			if runeWidth != writeWidth {
				panic("reading rune width not equal to the written rune width")
			}
			offset += writeWidth
		} else {
			break
		}
	}
	return b[:offset]
}

// FindReaderIndex returns a two-element slice of integers defining the
// location of the leftmost match of the regular expression in text read from
// the RuneReader. The match text was found in the input stream at
// byte offset loc[0] through loc[1]-1.
// A return value of nil indicates no match.
func (re *Regexp) FindReaderIndex(r io.RuneReader) []int {
	return re.FindIndex(fromReader(r))
}

// FindReaderSubmatchIndex returns a slice holding the index pairs
// identifying the leftmost match of the regular expression of text read by
// the RuneReader, and the matches, if any, of its subexpressions, as defined
// by the 'Submatch' and 'Index' descriptions in the package comment. A
// return value of nil indicates no match.
func (re *Regexp) FindReaderSubmatchIndex(r io.RuneReader) []int {
	return re.FindSubmatchIndex(fromReader(r))
}

// MatchReader reports whether the Regexp matches the text read by the
// RuneReader.
func (re *Regexp) MatchReader(r io.RuneReader) bool {
	return re.Match(fromReader(r))
}

// LiteralPrefix returns a literal string that must begin any match
// of the regular expression re. It returns the boolean true if the
// literal string comprises the entire regular expression.
func (re *Regexp) LiteralPrefix() (prefix string, complete bool) {
	panic("no easy way to implement this")
	// return "", false
}

// MatchString reports whether the Regexp matches the string s.
func MatchString(pattern string, s string) (matched bool, error error) {
	re, err := Compile(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}

// Gsub TODO DOKU
func (re *Regexp) Gsub(src, repl string) string {
	srcBytes := []byte(src)
	replBytes := []byte(repl)
	re.lock()
	replaced := re.replaceAll(srcBytes, replBytes, fillCapturedValues)
	re.unlock()
	return string(replaced)
}

// GsubFunc TODO DOKU
func (re *Regexp) GsubFunc(src string, replFunc func(string, map[string]string) string) string {
	srcBytes := []byte(src)
	re.lock()
	replaced := re.replaceAll(srcBytes, nil, func(_ []byte, matchBytes []byte, capturedBytes map[string][]byte) []byte {
		capturedStrings := make(map[string]string)
		for name, capBytes := range capturedBytes {
			capturedStrings[name] = string(capBytes)
		}
		matchString := string(matchBytes)
		return []byte(replFunc(matchString, capturedStrings))
	})
	re.unlock()
	return string(replaced)
}
