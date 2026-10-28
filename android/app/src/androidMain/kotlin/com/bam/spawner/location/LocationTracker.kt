package com.bam.spawner.location

import android.Manifest
import android.annotation.SuppressLint
import android.content.Context
import android.content.pm.PackageManager
import android.location.Location
import android.location.LocationListener
import android.location.LocationManager
import android.os.Build
import android.os.Looper
import androidx.core.content.ContextCompat
import com.bam.spawner.net.GeoFix
import com.bam.spawner.platformLog

/**
 * Keeps a cached most-recent location fix so a `wake` can be tagged with where the
 * clip was recorded — without ever waiting for a fix. Plain [LocationManager] (no
 * Google Play Services): [start] seeds the cache from every provider's last-known
 * fix and subscribes to low-power updates (passive + network, and the platform
 * `fused` provider where it exists — never a dedicated GPS request); [current]
 * returns the newest fix it can get without blocking. Every call is a no-op when
 * the location permission isn't granted, and none of them throw.
 */
class LocationTracker(context: Context) {
    private val app = context.applicationContext
    private val lm: LocationManager? = app.getSystemService(LocationManager::class.java)
    @Volatile private var latest: Location? = null
    @Volatile private var running = false

    private val listener = LocationListener { loc -> offer(loc) }

    /** Whether coarse or fine location is granted (either is enough to tag clips). */
    fun hasPermission(): Boolean =
        granted(Manifest.permission.ACCESS_FINE_LOCATION) || granted(Manifest.permission.ACCESS_COARSE_LOCATION)

    private fun granted(p: String) =
        ContextCompat.checkSelfPermission(app, p) == PackageManager.PERMISSION_GRANTED

    /** Begin low-power updates (idempotent). Must be called on a thread that may touch
     *  the main looper's handler — the callbacks themselves land on the main looper. */
    @SuppressLint("MissingPermission") // checked by hasPermission()
    fun start() {
        val lm = lm ?: return
        if (running || !hasPermission()) return
        seedFromLastKnown()
        var any = false
        for (p in UPDATE_PROVIDERS) {
            try {
                if (p !in lm.allProviders) continue
                lm.requestLocationUpdates(p, MIN_INTERVAL_MS, MIN_DISTANCE_M, listener, Looper.getMainLooper())
                any = true
            } catch (e: Exception) { // SecurityException / IllegalArgumentException — skip it
                platformLog("location: no updates from $p: ${e.message}")
            }
        }
        running = any
    }

    /** Stop updates (idempotent). The cached fix is kept — a later wake can still use it. */
    fun stop() {
        if (!running) return
        running = false
        try { lm?.removeUpdates(listener) } catch (_: Exception) {}
    }

    /**
     * The newest fix available right now, or null. Refreshes from the providers'
     * last-known fixes first (a cheap binder call, never a fresh fix request), so a
     * push-to-talk wake with the tracker stopped still gets whatever the OS has.
     */
    fun current(): GeoFix? {
        if (!hasPermission()) return null
        seedFromLastKnown()
        return latest?.toGeoFix()
    }

    @SuppressLint("MissingPermission")
    private fun seedFromLastKnown() {
        val lm = lm ?: return
        val providers = try { lm.allProviders } catch (_: Exception) { return }
        for (p in providers) {
            try { lm.getLastKnownLocation(p)?.let(::offer) } catch (_: Exception) {}
        }
    }

    /** Keep [loc] if it is newer than the cached fix (or meaningfully more accurate
     *  while about as fresh). */
    private fun offer(loc: Location) {
        val cur = latest
        if (cur == null || better(loc, cur)) latest = loc
    }

    private fun better(a: Location, b: Location): Boolean {
        val dt = a.time - b.time
        if (dt > FRESHNESS_TIE_MS) return true
        if (dt < -FRESHNESS_TIE_MS) return false
        // Within the tie window: prefer the tighter accuracy radius.
        val aa = if (a.hasAccuracy()) a.accuracy else Float.MAX_VALUE
        val ba = if (b.hasAccuracy()) b.accuracy else Float.MAX_VALUE
        return aa < ba || (aa == ba && dt > 0)
    }

    private fun Location.toGeoFix() = GeoFix(
        lat = latitude,
        lon = longitude,
        altM = if (hasAltitude()) altitude else null,
        accuracyM = if (hasAccuracy()) accuracy.toDouble() else null,
        fixAt = time,
        provider = provider ?: "unknown",
    )

    private companion object {
        /** Low-power sources only: passive (piggybacks other apps' fixes, free), network
         *  (Wi-Fi/cell), and the platform fused provider (API 31+) when present. */
        val UPDATE_PROVIDERS: List<String> = buildList {
            add(LocationManager.PASSIVE_PROVIDER)
            add(LocationManager.NETWORK_PROVIDER)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) add(LocationManager.FUSED_PROVIDER)
        }
        const val MIN_INTERVAL_MS = 60_000L
        const val MIN_DISTANCE_M = 25f
        /** Fixes this close in time compete on accuracy instead of age. */
        const val FRESHNESS_TIE_MS = 30_000L
    }
}
