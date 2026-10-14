package redact

import (
	"context"
	"fmt"
	"log/slog"
)

// NewHandler wraps next so configured secret values are removed from records.
func NewHandler(next slog.Handler, scrubber *Scrubber) slog.Handler {
	return &scrubbingHandler{next: next, scrubber: scrubber}
}

type scrubbingHandler struct {
	next     slog.Handler
	scrubber *Scrubber
}

func (h *scrubbingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *scrubbingHandler) Handle(ctx context.Context, record slog.Record) error {
	clean := slog.NewRecord(record.Time, record.Level, h.scrubber.Scrub(record.Message), record.PC)
	record.Attrs(func(attr slog.Attr) bool {
		clean.AddAttrs(h.scrubAttr(attr))
		return true
	})
	return h.next.Handle(ctx, clean)
}

func (h *scrubbingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(attrs))
	for i, attr := range attrs {
		clean[i] = h.scrubAttr(attr)
	}
	return &scrubbingHandler{next: h.next.WithAttrs(clean), scrubber: h.scrubber}
}

func (h *scrubbingHandler) WithGroup(name string) slog.Handler {
	return &scrubbingHandler{next: h.next.WithGroup(h.scrubber.Scrub(name)), scrubber: h.scrubber}
}

func (h *scrubbingHandler) scrubAttr(attr slog.Attr) slog.Attr {
	attr.Key = h.scrubber.Scrub(attr.Key)
	attr.Value = h.scrubValue(attr.Value)
	return attr
}

func (h *scrubbingHandler) scrubValue(value slog.Value) slog.Value {
	value = value.Resolve()
	switch value.Kind() {
	case slog.KindString:
		return slog.StringValue(h.scrubber.Scrub(value.String()))
	case slog.KindGroup:
		attrs := value.Group()
		for i := range attrs {
			attrs[i] = h.scrubAttr(attrs[i])
		}
		return slog.GroupValue(attrs...)
	case slog.KindAny:
		return slog.StringValue(h.scrubber.Scrub(fmt.Sprint(value.Any())))
	}
	return value
}
