package com.bam.spawner.tts

/**
 * Converts Claude's markdown replies into plain text for text-to-speech, so the
 * engine doesn't read out "asterisk asterisk word asterisk asterisk". The chat
 * log keeps the original markdown; only the spoken form is stripped.
 */
object Markdown {
    private val link = Regex("!?\\[([^\\]]*)]\\([^)]*\\)")
    private val codeFence = Regex("```[a-zA-Z0-9_+-]*")
    private val hrule = Regex("(?m)^\\s*([-*_])\\1{2,}\\s*$")
    private val linePrefix = Regex("(?m)^\\s{0,3}(#{1,6}\\s+|>\\s?|[-*+]\\s+|\\d+\\.\\s+)")
    private val dashes = Regex("-{2,}")
    private val spaces = Regex("[ \\t]+")
    private val blankLines = Regex("\\n{2,}")

    fun toSpeech(md: String): String {
        var s = md
        s = link.replace(s) { it.groupValues[1] }   // [text](url) -> text
        s = codeFence.replace(s, "")                 // ``` code fences
        s = hrule.replace(s, "")                     // --- / *** rules (whole line)
        s = linePrefix.replace(s, "")                // headings, quotes, bullets, "1."
        s = s.replace("**", "").replace("__", "").replace("~~", "")
        s = s.replace("*", "").replace("`", "").replace("~", "").replace("#", "")
        s = s.replace("_", " ")                      // snake_case -> words
        s = dashes.replace(s, " ")                   // -- inline -> space
        s = s.replace("|", " ")                      // table pipes
        s = s.replace("\\", "")                      // stray backslashes
        s = blankLines.replace(s, ". ")              // paragraph breaks -> pause
        s = spaces.replace(s, " ")
        return s.trim()
    }
}
