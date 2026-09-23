package agent

import (
	"context"
	"errors"
	"testing"

	"ora/internal/tracker"
)

// A session with a road for pictures is not blind: the look is pushed to the model out of band and the coordinates it reads off that picture resolve. Without this the Live voice session took a picture it could never be shown, so draw and click_at refused every time and it told the user its screen tools were broken.
func TestDeliverPicture_SentPictureIsNotBlind(t *testing.T) {
	var sent tracker.Capture
	ctx := WithPictureSender(liveScreenScope(context.Background()), func(_ context.Context, c tracker.Capture) error {
		sent = c
		return nil
	})
	shot := tracker.Capture{W: 1280, H: 698, Scale: 1.5, Data: []byte("png"), Mime: "image/png"}
	recordLook(ctx, shot)

	if !deliverPicture(ctx, shot) {
		t.Fatal("the picture was not delivered")
	}
	if sent.W != 1280 || string(sent.Data) != "png" {
		t.Errorf("sent = %+v, want the capture itself", sent)
	}
	got, ok, blind := lookSeen(ctx)
	if !ok || blind || got.W != 1280 {
		t.Errorf("lookSeen = (%+v, %v, blind %v), want the picture and not blind", got, ok, blind)
	}
}

// A send that fails leaves the session blind rather than claiming the model saw a picture it never got, because a coordinate read off an unseen picture is a guess.
func TestDeliverPicture_FailedSendStaysBlind(t *testing.T) {
	ctx := WithPictureSender(liveScreenScope(context.Background()), func(context.Context, tracker.Capture) error {
		return errors.New("socket closed")
	})
	shot := tracker.Capture{W: 1280, H: 698}
	recordLook(ctx, shot)

	if deliverPicture(ctx, shot) {
		t.Fatal("a failed send reported the picture delivered")
	}
	if _, ok, blind := lookSeen(ctx); ok || !blind {
		t.Errorf("lookSeen ok=%v blind=%v, want blind", ok, blind)
	}
}
