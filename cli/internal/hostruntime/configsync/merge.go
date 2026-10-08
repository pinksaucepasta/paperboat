package configsync

import (
	"bytes"
	"context"
	"errors"
	"unicode/utf8"
)

var ErrTextMergeConflict = errors.New("config text merge is not clean")

// Bound automatic merge effort independently of the file-size policy. A million
// line comparisons/trace cells handles ordinary config edits while keeping diff
// scratch storage to tens of MiB. Exhaustion preserves the existing conflict.
const mergeWorkLimit = 1 << 20

type mergeBudget struct {
	ctx       context.Context
	remaining int
}

func (b *mergeBudget) spend(n int) error {
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if n > b.remaining {
		return ErrTextMergeConflict
	}
	b.remaining -= n
	return nil
}

type textEdit struct {
	start, end int // Half-open byte offsets in the common base.
	value      []byte
}

func mergeRegularText(ctx context.Context, base, local, remote []byte, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if maxBytes < 1 || int64(len(base)) > maxBytes || int64(len(local)) > maxBytes ||
		int64(len(remote)) > maxBytes || !mergeText(base) || !mergeText(local) || !mergeText(remote) {
		return nil, ErrTextMergeConflict
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bytes.Equal(local, remote) || bytes.Equal(base, remote) {
		return cloneMergeResult(ctx, local)
	}
	if bytes.Equal(base, local) {
		return cloneMergeResult(ctx, remote)
	}
	budget := &mergeBudget{ctx: ctx, remaining: mergeWorkLimit}
	left, err := lineEdits(budget, base, local)
	if err != nil {
		return nil, err
	}
	right, err := lineEdits(budget, base, remote)
	if err != nil {
		return nil, err
	}
	output := &boundedMergeBuffer{limit: maxBytes}
	position := 0
	for len(left)+len(right) > 0 {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		start := len(base)
		if len(left) > 0 {
			start = left[0].start
		}
		if len(right) > 0 && right[0].start < start {
			start = right[0].start
		}
		end, endInsertion := start, true
		var a, b []textEdit
		// Group connected overlaps. Boundary insertions conflict conservatively;
		// adjacent replacements of distinct lines remain independent.
		for {
			useLeft := len(left) > 0 && (len(right) == 0 || left[0].start <= right[0].start)
			var next textEdit
			if useLeft {
				next = left[0]
			} else if len(right) > 0 {
				next = right[0]
			} else {
				break
			}
			if next.start > end || (next.start == end && next.end > next.start && !endInsertion) {
				break
			}
			if err := budget.spend(1); err != nil {
				return nil, err
			}
			if useLeft {
				a = append(a, next)
				left = left[1:]
			} else {
				b = append(b, next)
				right = right[1:]
			}
			if next.end > end {
				end = next.end
				endInsertion = false
			}
			if next.start == end && next.end == end {
				endInsertion = true
			}
		}
		if _, err := output.Write(base[position:start]); err != nil {
			return nil, err
		}
		var value []byte
		if len(a) == 0 {
			value, err = renderEdits(budget, base, start, end, b, maxBytes)
		} else {
			value, err = renderEdits(budget, base, start, end, a, maxBytes)
			if err == nil && len(b) > 0 {
				var other []byte
				other, err = renderEdits(budget, base, start, end, b, maxBytes)
				if err == nil && !bytes.Equal(value, other) {
					err = ErrTextMergeConflict
				}
			}
		}
		if err != nil {
			return nil, err
		}
		if _, err = output.Write(value); err != nil {
			return nil, err
		}
		position = end
	}
	if _, err = output.Write(base[position:]); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func cloneMergeResult(ctx context.Context, value []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := bytes.Clone(value)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func renderEdits(budget *mergeBudget, base []byte, start, end int, edits []textEdit, maxBytes int64) ([]byte, error) {
	out := &boundedMergeBuffer{limit: maxBytes}
	for _, edit := range edits {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		if _, err := out.Write(base[start:edit.start]); err != nil {
			return nil, err
		}
		if _, err := out.Write(edit.value); err != nil {
			return nil, err
		}
		start = edit.end
	}
	if _, err := out.Write(base[start:end]); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// lineEdits computes a shortest line edit script using Myers' frontier/trace.
// Unlike an external process or a soft diff timeout, all scans and frontier
// allocations consume the shared work budget and observe cancellation.
func lineEdits(budget *mergeBudget, base, side []byte) ([]textEdit, error) {
	start, baseEnd, sideEnd := 0, len(base), len(side)
	for start < baseEnd && start < sideEnd {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		x, y := nextLine(base, start), nextLine(side, start)
		if !bytes.Equal(base[start:x], side[start:y]) {
			break
		}
		start = x
	}
	for baseEnd > start && sideEnd > start {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		x, y := previousLine(base, start, baseEnd), previousLine(side, start, sideEnd)
		if !bytes.Equal(base[x:baseEnd], side[y:sideEnd]) {
			break
		}
		baseEnd, sideEnd = x, y
	}
	if start == baseEnd || start == sideEnd {
		if start == baseEnd && start == sideEnd {
			return nil, nil
		}
		return []textEdit{{start: start, end: baseEnd, value: side[start:sideEnd]}}, nil
	}
	a, err := lineOffsets(budget, base[start:baseEnd])
	if err != nil {
		return nil, err
	}
	b, err := lineOffsets(budget, side[start:sideEnd])
	if err != nil {
		return nil, err
	}
	n, m := len(a)-1, len(b)-1
	var trace [][]int
	for d := 0; d <= n+m; d++ {
		if err := budget.spend(2*d + 1); err != nil {
			return nil, err
		}
		front := make([]int, 2*d+1)
		for k := -d; k <= d; k += 2 {
			if err := budget.spend(1); err != nil {
				return nil, err
			}
			x := 0
			if d > 0 {
				prev := trace[d-1]
				if k == -d || (k != d && prev[k-1+d-1] < prev[k+1+d-1]) {
					x = prev[k+1+d-1]
				} else {
					x = prev[k-1+d-1] + 1
				}
			}
			y := x - k
			for x < n && y < m {
				if err := budget.spend(1); err != nil {
					return nil, err
				}
				if !bytes.Equal(base[start+a[x]:start+a[x+1]], side[start+b[y]:start+b[y+1]]) {
					break
				}
				x++
				y++
			}
			front[k+d] = x
			if x >= n && y >= m {
				trace = append(trace, front)
				return editsFromTrace(budget, trace, a, b, start, side)
			}
		}
		trace = append(trace, front)
	}
	return nil, ErrTextMergeConflict
}

func nextLine(value []byte, start int) int {
	if n := bytes.IndexByte(value[start:], '\n'); n >= 0 {
		return start + n + 1
	}
	return len(value)
}
func previousLine(value []byte, start, end int) int {
	if value[end-1] == '\n' {
		end--
	}
	return start + bytes.LastIndexByte(value[start:end], '\n') + 1
}
func lineOffsets(budget *mergeBudget, value []byte) ([]int, error) {
	offsets := []int{0}
	for pos := 0; pos < len(value); {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		pos = nextLine(value, pos)
		offsets = append(offsets, pos)
	}
	return offsets, nil
}

type diffStep struct {
	kind  byte
	count int
}

func editsFromTrace(budget *mergeBudget, trace [][]int, a, b []int, start int, side []byte) ([]textEdit, error) {
	x, y := len(a)-1, len(b)-1
	var steps []diffStep
	for d := len(trace) - 1; d > 0; d-- {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		k := x - y
		prev := trace[d-1]
		previousK := k - 1
		if k == -d || (k != d && prev[k-1+d-1] < prev[k+1+d-1]) {
			previousK = k + 1
		}
		px := prev[previousK+d-1]
		py := px - previousK
		matched := 0
		for x > px && y > py {
			if err := budget.spend(1); err != nil {
				return nil, err
			}
			x--
			y--
			matched++
		}
		if matched > 0 {
			steps = append(steps, diffStep{'=', matched})
		}
		if x == px {
			steps = append(steps, diffStep{'+', 1})
			y--
		} else {
			steps = append(steps, diffStep{'-', 1})
			x--
		}
	}
	if x > 0 {
		steps = append(steps, diffStep{'=', x})
	}
	var edits []textEdit
	basePos, sidePos, editBase, editSide := 0, 0, -1, 0
	flush := func() {
		if editBase >= 0 {
			edits = append(edits, textEdit{start: start + a[editBase], end: start + a[basePos], value: side[start+b[editSide] : start+b[sidePos]]})
			editBase = -1
		}
	}
	for i := len(steps) - 1; i >= 0; i-- {
		if err := budget.spend(1); err != nil {
			return nil, err
		}
		s := steps[i]
		if s.kind == '=' {
			flush()
			basePos += s.count
			sidePos += s.count
			continue
		}
		if editBase < 0 {
			editBase, editSide = basePos, sidePos
		}
		if s.kind == '-' {
			basePos++
		} else {
			sidePos++
		}
	}
	flush()
	return edits, nil
}

func mergeText(value []byte) bool { return utf8.Valid(value) && !bytes.ContainsRune(value, 0) }

type boundedMergeBuffer struct {
	bytes.Buffer
	limit int64
}

func (b *boundedMergeBuffer) Write(value []byte) (int, error) {
	if int64(len(value)) > b.limit-int64(b.Len()) {
		return 0, ErrTextMergeConflict
	}
	return b.Buffer.Write(value)
}
