package com.bam.spawner

import androidx.compose.ui.text.style.TextAlign
import com.bam.spawner.ui.MdParagraph
import com.bam.spawner.ui.MdTable
import com.bam.spawner.ui.fitColumns
import com.bam.spawner.ui.parseBlocks
import com.bam.spawner.ui.splitTableRow
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

/**
 * The markdown block parser's table handling. Before tables were understood, the
 * rows fell through to the paragraph branch and were joined with spaces — the
 * "garbled pipes on one line" bug these tests pin down.
 */
class MarkdownTableTest {
    @Test
    fun parsesAPipeTable() {
        val blocks = parseBlocks(
            """
            | Name | Size |
            |------|------|
            | a    | 1    |
            | b    | 2    |
            """.trimIndent()
        )
        assertEquals(1, blocks.size)
        val t = blocks[0] as MdTable
        assertEquals(listOf("Name", "Size"), t.header)
        assertEquals(listOf(listOf("a", "1"), listOf("b", "2")), t.rows)
    }

    @Test
    fun readsAlignmentColons() {
        val t = parseBlocks("| a | b | c |\n|:--|:-:|--:|\n| 1 | 2 | 3 |")[0] as MdTable
        assertEquals(listOf(TextAlign.Start, TextAlign.Center, TextAlign.End), t.aligns)
    }

    @Test
    fun acceptsTablesWithoutOuterPipes() {
        val t = parseBlocks("a | b\n--- | ---\n1 | 2")[0] as MdTable
        assertEquals(listOf("a", "b"), t.header)
        assertEquals(listOf(listOf("1", "2")), t.rows)
    }

    @Test
    fun separatesSurroundingProse() {
        val blocks = parseBlocks("before\n| a | b |\n|---|---|\n| 1 | 2 |\n\nafter")
        assertEquals(MdParagraph("before"), blocks[0])
        assertTrue(blocks[1] is MdTable)
        assertEquals(MdParagraph("after"), blocks[2])
    }

    // A pipe in a sentence followed by a `---` rule is NOT a one-column table: the
    // separator's cell count has to match the header's.
    @Test
    fun ignoresProsePipeFollowedByHorizontalRule() {
        val blocks = parseBlocks("use a | b here\n---\nnext")
        assertTrue(blocks.none { it is MdTable })
    }

    @Test
    fun keepsEscapedPipesInsideCells() {
        assertEquals(listOf("a|b", "c"), splitTableRow("| a\\|b | c |"))
    }

    @Test
    fun raggedRowsSurvive() {
        val t = parseBlocks("| a | b | c |\n|---|---|---|\n| 1 | 2 |")[0] as MdTable
        assertEquals(listOf(listOf("1", "2")), t.rows)
    }

    @Test
    fun fitColumnsLeavesNarrowTablesAlone() {
        assertEquals(listOf(30, 50), fitColumns(intArrayOf(30, 50), 200).toList())
    }

    @Test
    fun fitColumnsShrinksToFitAndFloorsSmallColumns() {
        // Natural 900 into 300: proportional shares are 266/33, but the second column
        // is floored at half an even share (75), so both stay legible and the total fits.
        val out = fitColumns(intArrayOf(800, 100), 300)
        assertEquals(75, out[1])
        assertTrue(out.sum() <= 300, "total ${out.sum()} exceeded 300")
        assertTrue(out[0] > out[1], "wide column should stay widest")
    }
}
