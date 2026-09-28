// picture.go carries a screenshot to a model that cannot be handed one as a tool result.
// The text brains take a look's picture through takeLook, which folds it into the request they are assembling. The Live voice session cannot: its tool results go back as text, so every picture it took stayed undelivered and draw and click_at refused for want of coordinates — see cannotSeePictures. The Live API does have a road for images, the same SendRealtimeInput the microphone uses every frame, but it is not the tool-result road, so the look tool has to push the picture down it itself. This file is that push, attached to the context the way every other per-ask capability in this package is.
package agent

import (
	"context"
	"log/slog"

	"june/internal/tracker"
)

// PictureSender hands one screenshot to the model out of band. Input: the ask's context and the capture. Output: an error when the picture could not be sent, which leaves the session blind rather than pretending it saw something.
type PictureSender func(context.Context, tracker.Capture) error

type pictureSenderKey struct{}

// WithPictureSender attaches fn to ctx so a look taken under it reaches the model. Input: the parent context and the sender, which may be nil on a channel that has no such road. Output: a context carrying it.
func WithPictureSender(ctx context.Context, fn PictureSender) context.Context {
	return context.WithValue(ctx, pictureSenderKey{}, fn)
}

// pictureSenderFrom reads the sender attached to ctx. Output: the sender, or nil when this channel has none, which is every text ask — those deliver through takeLook instead.
func pictureSenderFrom(ctx context.Context) PictureSender {
	fn, _ := ctx.Value(pictureSenderKey{}).(PictureSender)
	return fn
}

// deliverPicture pushes the capture to the model when this channel has a road for one, and records that the model has seen it. Input: the ask's context and the picture just taken. Output: true when the model was shown it.
// A send that fails is not recorded as delivered: the picture is only usable as a frame of reference if the model actually holds it, and a coordinate read off one it never saw is a guess.
func deliverPicture(ctx context.Context, c tracker.Capture) bool {
	send := pictureSenderFrom(ctx)
	if send == nil {
		return false
	}
	if err := send(ctx, c); err != nil {
		slog.Warn("failed to deliver the look's picture to the voice session", "error", err)
		return false
	}
	markLookDelivered(ctx)
	return true
}
