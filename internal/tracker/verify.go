package tracker

import (
	"errors"
	"fmt"

	"june/internal/act"
)

// Relabelled reports that the element is still there, still the same role, and still answering on the same reference, but carries a different name than the list recorded. It is a distinct type because it is the one difference a caller can carry on through: a Play button that now says Pause is the same button, and the only thing that must change is which name the stop line is judged against.
type Relabelled struct{ Now, Was string }

func (e *Relabelled) Error() string {
	return fmt.Sprintf("it is now labelled %q, not %q", e.Now, e.Was)
}

// ErrActionUnconfirmed is what DoAction wraps when the action was sent and may well have happened, but did not report back in time. A control whose action opens a modal dialog may not return until the dialog is closed, so a caller that took this for "nothing fired" and clicked the element with the pointer pressed it twice, or clicked into the dialog it had opened.
var ErrActionUnconfirmed = errors.New("the action was sent but did not report back in time")

// VerifyAgainst compares what an element is now with what observe_screen recorded for it. Input: the role and label read from the element just now, then the role and label the numbered list showed. Output: nil when they still describe the same element, a *Relabelled when only the name changed, or an error naming what else changed; an empty label in the list means the list held none, and a content role's label is not compared at all.
// Exported so an end-to-end test can put a fake accessibility read through the same decision the bus-backed one makes, rather than a second copy of the rule that can drift from it.
func VerifyAgainst(nowRole, nowLabel string, role, label string) error {
	// AT-SPI reports a node whose widget was destroyed as role "invalid", which is the element gone, not a new role.
	if nowRole == "" || nowRole == "invalid" {
		return errors.New("the element has gone")
	}
	if nowRole != role {
		return fmt.Errorf("it is now a %s, not a %s", nowRole, role)
	}
	// A content role's label is the node's own contents — what is typed in an entry, what a run of page text says — not a name anybody chose for it, so it changes whenever the user types and says nothing about whether this is still the same element. The role still does. Comparing it refused a click on a box the user had just typed into, which is the ordinary thing to happen between listing a box and clicking it.
	if label != "" && !act.ContentRole(role) && nowLabel != label {
		return &Relabelled{Now: nowLabel, Was: label}
	}
	return nil
}
