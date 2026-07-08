package com.bam.spawner.ui

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material3.LocalContentColor
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.AnnotatedString
import androidx.compose.ui.text.SpanStyle
import androidx.compose.ui.text.buildAnnotatedString
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.text.withStyle
import androidx.compose.ui.unit.dp

/**
 * Renders Claude's markdown natively in Compose — headings, bold/italic, inline
 * code, fenced code blocks, bullet/numbered lists, and links. Deliberately small
 * (no external dependency); covers the common cases Claude produces.
 */
@Composable
fun MarkdownText(text: String, modifier: Modifier = Modifier) {
    val blocks = remember(text) { parseBlocks(text) }
    Column(modifier, verticalArrangement = Arrangement.spacedBy(4.dp)) {
        for (b in blocks) {
            when (b) {
                is MdHeader -> Text(
                    inline(b.text),
                    style = when (b.level) {
                        1 -> MaterialTheme.typography.titleLarge
                        2 -> MaterialTheme.typography.titleMedium
                        else -> MaterialTheme.typography.titleSmall
                    },
                    fontWeight = FontWeight.Bold,
                )
                is MdCode -> Surface(
                    color = LocalContentColor.current.copy(alpha = 0.10f),
                    shape = RoundedCornerShape(6.dp),
                    modifier = Modifier.fillMaxWidth(),
                ) {
                    Text(
                        b.text, Modifier.padding(8.dp),
                        fontFamily = FontFamily.Monospace,
                        style = MaterialTheme.typography.bodySmall,
                    )
                }
                is MdBullet -> Row {
                    Text("•  ")
                    Text(inline(b.text), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                }
                is MdNumbered -> Row {
                    Text("${b.marker}  ")
                    Text(inline(b.text), Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
                }
                is MdParagraph -> Text(inline(b.text), style = MaterialTheme.typography.bodyMedium)
            }
        }
    }
}

private sealed interface MdBlock
private data class MdParagraph(val text: String) : MdBlock
private data class MdHeader(val level: Int, val text: String) : MdBlock
private data class MdCode(val text: String) : MdBlock
private data class MdBullet(val text: String) : MdBlock
private data class MdNumbered(val marker: String, val text: String) : MdBlock

private val bulletRe = Regex("^[-*+]\\s+")
private val numRe = Regex("^(\\d+)\\.\\s+")

private fun parseBlocks(md: String): List<MdBlock> {
    val blocks = mutableListOf<MdBlock>()
    val lines = md.split("\n")
    val para = StringBuilder()
    fun flush() {
        if (para.isNotBlank()) blocks.add(MdParagraph(para.trim().toString()))
        para.clear()
    }
    var i = 0
    while (i < lines.size) {
        val line = lines[i]
        val t = line.trimStart()
        when {
            t.startsWith("```") -> {
                flush()
                val sb = StringBuilder()
                i++
                while (i < lines.size && !lines[i].trimStart().startsWith("```")) {
                    sb.append(lines[i]).append('\n'); i++
                }
                i++ // skip closing fence
                blocks.add(MdCode(sb.toString().trimEnd('\n')))
                continue
            }
            t.startsWith("#") -> {
                flush()
                val hashes = t.takeWhile { it == '#' }.length
                blocks.add(MdHeader(hashes.coerceIn(1, 6), t.drop(hashes).trim()))
            }
            bulletRe.containsMatchIn(t) -> {
                flush(); blocks.add(MdBullet(t.replaceFirst(bulletRe, "")))
            }
            numRe.containsMatchIn(t) -> {
                flush()
                val m = numRe.find(t)!!
                blocks.add(MdNumbered(m.groupValues[1] + ".", t.substring(m.value.length)))
            }
            t.isBlank() -> flush()
            else -> {
                if (para.isNotEmpty()) para.append(' ')
                para.append(t)
            }
        }
        i++
    }
    flush()
    return blocks
}

/** Parses inline markdown (**bold**, *italic*, `code`, [text](url)) to styled text. */
@Composable
private fun inline(text: String): AnnotatedString {
    val codeBg = LocalContentColor.current.copy(alpha = 0.12f)
    val linkColor = MaterialTheme.colorScheme.primary
    return remember(text, codeBg, linkColor) {
        buildAnnotatedString {
            var i = 0
            while (i < text.length) {
                when {
                    text.startsWith("**", i) || text.startsWith("__", i) -> {
                        val marker = text.substring(i, i + 2)
                        val end = text.indexOf(marker, i + 2)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontWeight = FontWeight.Bold)) { append(text.substring(i + 2, end)) }
                            i = end + 2
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '*' || text[i] == '_' -> {
                        val end = text.indexOf(text[i], i + 1)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontStyle = FontStyle.Italic)) { append(text.substring(i + 1, end)) }
                            i = end + 1
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '`' -> {
                        val end = text.indexOf('`', i + 1)
                        if (end >= 0) {
                            withStyle(SpanStyle(fontFamily = FontFamily.Monospace, background = codeBg)) {
                                append(text.substring(i + 1, end))
                            }
                            i = end + 1
                        } else { append(text[i]); i++ }
                    }
                    text[i] == '[' -> {
                        val close = text.indexOf(']', i)
                        if (close >= 0 && close + 1 < text.length && text[close + 1] == '(') {
                            val paren = text.indexOf(')', close + 2)
                            if (paren >= 0) {
                                withStyle(SpanStyle(color = linkColor, textDecoration = TextDecoration.Underline)) {
                                    append(text.substring(i + 1, close))
                                }
                                i = paren + 1
                            } else { append(text[i]); i++ }
                        } else { append(text[i]); i++ }
                    }
                    else -> { append(text[i]); i++ }
                }
            }
        }
    }
}
