pluginManagement {
    repositories {
        google()
        mavenCentral()
        gradlePluginPortal()
    }
}
dependencyResolutionManagement {
    // No strict repositoriesMode: the Kotlin/Wasm toolchain injects its own project-level
    // download repositories (binaryen from GitHub releases, node, yarn). FAIL_ON_PROJECT_REPOS
    // / PREFER_SETTINGS reject or shadow those and the wasm compiler can't be fetched, so we let
    // project repos stand and declare the real dependency repos here for everything else.
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "claude_spawner"
include(":app")
