package text

import "regexp"

// NonSpeechLine matches whisper's marker for a stretch with no words in it — "[BLANK_AUDIO]", "(upbeat music)", "[SOUND]". A quiet stream, whether a dictation into a silent room or the microphone side of a call, is otherwise nothing but these.
var NonSpeechLine = regexp.MustCompile(`^[\[(][^)\]]*[)\]]$`)
