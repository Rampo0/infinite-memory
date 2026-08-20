package textutil

// stopwords are excluded from tokens: English function words plus chat filler
// that carries no retrieval signal in coding conversations.
var stopwords = map[string]struct{}{}

func init() {
	words := []string{
		// articles, pronouns, prepositions, conjunctions, auxiliaries
		"a", "an", "the", "and", "or", "but", "nor", "so", "yet", "if", "then",
		"else", "when", "while", "where", "which", "who", "whom", "whose", "what",
		"why", "how", "that", "this", "these", "those", "there", "here",
		"i", "me", "my", "mine", "we", "us", "our", "ours", "you", "your", "yours",
		"he", "him", "his", "she", "her", "hers", "it", "its", "they", "them",
		"their", "theirs", "am", "is", "are", "was", "were", "be", "been", "being",
		"do", "does", "did", "doing", "done", "have", "has", "had", "having",
		"will", "would", "shall", "should", "can", "could", "may", "might", "must",
		"of", "in", "on", "at", "by", "for", "with", "about", "against", "between",
		"into", "through", "during", "before", "after", "above", "below", "to",
		"from", "up", "down", "out", "off", "over", "under", "again", "further",
		"once", "as", "than", "too", "very", "not", "no", "only", "own", "same",
		"such", "both", "each", "few", "more", "most", "other", "some", "any",
		"all", "also", "because", "until", "via", "per", "etc",
		// chat filler / generic dev vocabulary with no retrieval signal
		"please", "help", "want", "need", "make", "just", "like", "thing",
		"things", "stuff", "way", "let", "lets", "get", "got", "know", "think",
		"see", "look", "sure", "okay", "ok", "yes", "yeah", "thanks", "thank",
		"now", "new", "one", "two", "still", "well", "good", "right", "really",
		"actually", "basically", "maybe", "something", "anything", "everything",
		"work", "works", "working", "use", "using", "used", "fix", "add",
		"change", "update", "run", "running", "code", "file", "files", "error",
		"issue", "problem", "question", "hey", "hi", "hello",
	}
	for _, w := range words {
		stopwords[w] = struct{}{}
	}
}

// IsStopword reports whether the (already lowercased) token is a stopword.
func IsStopword(t string) bool {
	_, ok := stopwords[t]
	return ok
}
