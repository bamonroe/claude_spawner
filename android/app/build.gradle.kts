import org.jetbrains.kotlin.gradle.ExperimentalWasmDsl
import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.multiplatform")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.compose")
}

kotlin {
    androidTarget {
        compilerOptions {
            jvmTarget.set(JvmTarget.JVM_17)
        }
    }

    @OptIn(ExperimentalWasmDsl::class)
    wasmJs {
        moduleName = "spawnerweb"
        browser {
            commonWebpackConfig {
                outputFileName = "spawnerweb.js"
            }
        }
        binaries.executable()
    }

    sourceSets {
        // The generated command list (see generateCommands below) is shared UI data.
        commonMain {
            kotlin.srcDir(layout.buildDirectory.dir("generated/commands"))
        }
        commonMain.dependencies {
            implementation(compose.runtime)
            implementation(compose.foundation)
            implementation(compose.material3)
            implementation(compose.ui)
            implementation(compose.components.uiToolingPreview)
            implementation("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.8.1")
        }
        androidMain.dependencies {
            implementation("androidx.core:core-ktx:1.13.1")
            implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.6")
            implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.6")
            implementation("androidx.activity:activity-compose:1.9.3")
            implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.8.1")
            implementation(compose.uiTooling)
            // WebSocket transport to the server (Android target; web uses Ktor — see M2).
            implementation("com.squareup.okhttp3:okhttp:4.12.0")
        }
    }
}

android {
    namespace = "com.bam.spawner"
    compileSdk = 35

    defaultConfig {
        applicationId = "com.bam.spawner"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
        versionName = "0.1.0"
    }

    // Kotlin Multiplatform maps its `androidMain` kotlin source set to the Android `main`
    // variant, but AGP still looks under src/main for the manifest/res — point it at androidMain.
    sourceSets.getByName("main") {
        manifest.srcFile("src/androidMain/AndroidManifest.xml")
        res.srcDirs("src/androidMain/res")
    }

    signingConfigs {
        // Pin the debug signing key when SPAWNER_DEBUG_KEYSTORE points at one, so
        // containerized builds (which otherwise mint a random debug key each time)
        // produce a stable signature and `adb install -r` upgrades in place.
        getByName("debug") {
            val ks = System.getenv("SPAWNER_DEBUG_KEYSTORE")
            if (ks != null && file(ks).exists()) {
                storeFile = file(ks)
                storePassword = "android"
                keyAlias = "androiddebugkey"
                keyPassword = "android"
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

// generateCommands turns the shared docs/commands.json (emitted from the server's
// authoritative command registry) into a Kotlin COMMANDS list, so the app's
// command reference + alias editor can never drift from the server grammar and
// no command can ship undocumented. Runs before every build.
val generateCommands by tasks.registering {
    val jsonFile = rootProject.file("../docs/commands.json")
    val outDir = layout.buildDirectory.dir("generated/commands").get().asFile
    inputs.file(jsonFile)
    outputs.dir(outDir)
    doLast {
        fun esc(s: String) = s.replace("\\", "\\\\").replace("\"", "\\\"")
        @Suppress("UNCHECKED_CAST")
        val data = groovy.json.JsonSlurper().parse(jsonFile) as Map<String, Any>
        @Suppress("UNCHECKED_CAST")
        val cmds = (data["commands"] as List<Map<String, Any>>)
            .sortedBy { it["title"] as String } // alphabetical, defensively (JSON is already sorted)
        val sb = StringBuilder()
        sb.appendLine("// GENERATED from docs/commands.json by the generateCommands Gradle task.")
        sb.appendLine("// Do not edit — change the server command registry and run `go run ./cmd/gencommands`.")
        sb.appendLine("package com.bam.spawner")
        sb.appendLine()
        sb.appendLine("/** One \"hey buddy\" command: display name, spoken phrasings, description. */")
        sb.appendLine("data class Command(val name: String, val aliases: List<String>, val description: String)")
        sb.appendLine()
        sb.appendLine("/** Every control command, alphabetical. Source of truth: server command registry. */")
        sb.appendLine("val COMMANDS: List<Command> = listOf(")
        for (c in cmds) {
            @Suppress("UNCHECKED_CAST")
            val aliases = (c["aliases"] as List<String>).joinToString(", ") { "\"${esc(it)}\"" }
            sb.appendLine("    Command(\"${esc(c["title"] as String)}\", listOf($aliases), \"${esc(c["description"] as String)}\"),")
        }
        sb.appendLine(")")
        val pkgDir = outDir.resolve("com/bam/spawner").apply { mkdirs() }
        pkgDir.resolve("Commands.kt").writeText(sb.toString())
    }
}

// Every Kotlin compilation (Android + wasmJs) needs the generated COMMANDS source present first.
tasks.withType<org.jetbrains.kotlin.gradle.tasks.KotlinCompilationTask<*>>().configureEach {
    dependsOn(generateCommands)
}
