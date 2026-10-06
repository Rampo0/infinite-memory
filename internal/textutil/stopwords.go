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
		// Indonesian function words and chat filler (prompts are often
		// Indonesian, memories mostly English). Function words only: domain
		// vocabulary — rekening, akun, nasabah, dana, saham — must survive.
		"yang", "yg", "ini", "itu", "dan", "di", "ke", "dari", "untuk", "utk", "buat",
		"ada", "akan", "adalah", "ialah", "saya", "aku", "gw", "gue", "gua", "kamu",
		"lu", "lo", "kita", "kami", "mereka", "dia", "ia", "nya", "apa", "apakah",
		"adakah", "gimana", "bagaimana", "kenapa", "mengapa", "kapan", "dimana",
		"mana", "siapa", "berapa", "tolong", "mohon", "dong", "deh", "sih", "kok",
		"kan", "lah", "ya", "yah", "nih", "tuh", "aja", "saja", "udah", "sudah",
		"belum", "blm", "bisa", "mau", "ingin", "pengen", "kalau", "kalo", "klo",
		"jika", "jadi", "jd", "terus", "trus", "lalu", "lagi", "lg", "juga", "jg",
		"dengan", "dgn", "atau", "tapi", "tetapi", "namun", "karena", "krn", "karna",
		"soalnya", "gak", "ga", "nggak", "ngga", "enggak", "tidak", "tdk", "bukan",
		"setiap", "tiap", "maka", "sebelum", "sesudah", "setelah", "sekarang",
		"skrng", "skrg", "tadi", "nanti", "masih", "sedang", "pada", "oleh",
		"sebagai", "seperti", "kayak", "dalam", "kedalam", "atas", "bawah", "antara",
		"hal", "cara", "coba", "cek", "tanya", "pastikan", "sebut", "benar", "bener",
		"betul", "dulu", "dahulu", "terlebih", "sebelumnya", "berikut", "tersebut",
		"begitu", "gitu", "gini", "begini", "sini", "situ", "sana", "hanya", "cuma",
		"semua", "sama", "punya", "harus", "perlu", "boleh", "bikin", "hasil",
		"sendiri", "selesai", "saran", "nah", "oke", "sip", "banget", "bgt", "sangat", "amat",
		"kendala", "masalah", // issue / problem, stopwords in English too
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
