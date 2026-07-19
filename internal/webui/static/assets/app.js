/* Beeline operator console.
 *
 * Two jobs, one screen:
 *   1. Configure the service area — center (click/drag on the map), H3 resolution,
 *      area rings, radius rings — with a live H3 tessellation preview and an exact
 *      cell/pair count, then POST /_config_/area to rebuild the cache.
 *   2. Visualize cache-load progress — an aggregate bar + metrics from
 *      /_ops_/freshness, and a per-cell freshness overlay from /_ops_/cells.
 *
 * h3-js (window.h3) does all client-side tessellation so the preview matches the
 * server's tessellate.Seed exactly (same gridDisk + in-area clip). Leaflet renders
 * the map; tiles come from OSM (needs internet — cells/markers still draw offline).
 */
(function () {
  "use strict";

  var h3 = window.h3;
  var L = window.L;

  // ---- State ----------------------------------------------------------------
  var center = { lat: 37.7749, lng: -122.4194 };
  var profiles = [];
  var maskResolution = null;   // resolution the road mask is built at, or null if none
  var freshPolys = {};   // cellId -> Leaflet polygon, for in-place restyle
  var map, centerMarker, previewLayer, freshnessLayer;
  var toastTimer;

  var els = {
    centerVal: id("center-val"),
    centerCoords: id("center-coords"),
    res: id("res"),
    resVal: id("res-val"),
    resHint: id("res-hint"),
    maskWarn: id("mask-warn"),
    areaRings: id("area-rings"),
    areaRingsVal: id("area-rings-val"),
    radiusRings: id("radius-rings"),
    radiusRingsVal: id("radius-rings-val"),
    previewCells: id("preview-cells"),
    previewPairs: id("preview-pairs"),
    apply: id("apply"),
    pct: id("pct"),
    frac: id("frac"),
    barFill: id("bar-fill"),
    mDebt: id("m-debt"),
    mOldest: id("m-oldest"),
    mThroughput: id("m-throughput"),
    mThroughputBox: id("m-throughput-box"),
    mEta: id("m-eta"),
    profiles: id("profiles"),
    toast: id("toast"),
  };

  function id(x) { return document.getElementById(x); }

  // Approximate ground resolution (edge length, meters) per H3 res, for the hint.
  var RES_EDGE_M = {
    4: 22600, 5: 8540, 6: 3230, 7: 1220, 8: 461, 9: 174, 10: 65.9, 11: 24.9, 12: 9.4,
  };

  // ---- Map ------------------------------------------------------------------
  function initMap() {
    map = L.map("map", { zoomControl: true, attributionControl: true }).setView(
      [center.lat, center.lng],
      12
    );

    L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 19,
      attribution: "&copy; OpenStreetMap contributors",
    }).addTo(map);

    freshnessLayer = L.layerGroup().addTo(map);
    previewLayer = L.layerGroup().addTo(map);

    centerMarker = L.marker([center.lat, center.lng], {
      draggable: true,
      icon: L.divIcon({ className: "", html: '<div class="center-dot"></div>', iconSize: [14, 14] }),
    }).addTo(map);

    centerMarker.on("drag", function (e) {
      setCenter(e.target.getLatLng(), false);
    });
    centerMarker.on("dragend", renderPreview);

    map.on("click", function (e) {
      setCenter(e.latlng, true);
      renderPreview();
    });
  }

  function setCenter(latlng, moveMarker) {
    center = { lat: latlng.lat, lng: latlng.lng };
    if (moveMarker) centerMarker.setLatLng(latlng);
    els.centerVal.textContent = center.lat.toFixed(4) + ", " + center.lng.toFixed(4);
    els.centerCoords.textContent = center.lat.toFixed(5) + ", " + center.lng.toFixed(5);
  }

  // ---- Controls -------------------------------------------------------------
  function controlValues() {
    return {
      lat: center.lat,
      lng: center.lng,
      resolution: parseInt(els.res.value, 10),
      areaRings: parseInt(els.areaRings.value, 10),
      radiusRings: parseInt(els.radiusRings.value, 10),
    };
  }

  function bindControls() {
    [els.res, els.areaRings, els.radiusRings].forEach(function (input) {
      input.addEventListener("input", function () {
        els.resVal.textContent = els.res.value;
        els.areaRingsVal.textContent = els.areaRings.value;
        els.radiusRingsVal.textContent = els.radiusRings.value;
        updateResHint();
        updateMaskWarn();
        renderPreview();
      });
    });
    els.apply.addEventListener("click", applyArea);
  }

  function updateResHint() {
    var r = parseInt(els.res.value, 10);
    var e = RES_EDGE_M[r];
    els.resHint.textContent = e ? "≈ " + fmtMeters(e) + " cell edge" : "";
  }

  // Warn when the selected resolution is finer than the mask's: the road mask then
  // prunes only approximately (the pruned edge is as sharp as the mask, so water
  // cells near roads survive). See tessellate.Mask.Allows.
  function updateMaskWarn() {
    var r = parseInt(els.res.value, 10);
    if (maskResolution != null && r > maskResolution) {
      els.maskWarn.textContent =
        "⚠ Road mask is res " + maskResolution + ". At res " + r +
        " roadless cells are pruned only approximately — the water edge stays as coarse " +
        "as res " + maskResolution + ". Rebuild the mask at res " + r + " for a crisp edge.";
      els.maskWarn.hidden = false;
    } else {
      els.maskWarn.hidden = true;
    }
  }

  // ---- Preview (client-side tessellation, mirrors tessellate.Seed) ----------
  function renderPreview() {
    previewLayer.clearLayers();
    var v = controlValues();

    var originCell;
    try {
      originCell = h3.latLngToCell(v.lat, v.lng, v.resolution);
    } catch (err) {
      return;
    }

    var cells = h3.gridDisk(originCell, v.areaRings);
    var inArea = Object.create(null);
    cells.forEach(function (c) { inArea[c] = true; });

    // Outline each area cell (dashed accent, faint fill) as the pending boundary.
    cells.forEach(function (c) {
      L.polygon(h3.cellToBoundary(c), {
        color: "#ffb454",
        weight: 1,
        opacity: 0.55,
        fillColor: "#ffb454",
        fillOpacity: 0.05,
        dashArray: "3,3",
        interactive: false,
      }).addTo(previewLayer);
    });

    els.previewCells.textContent = cells.length.toLocaleString();
    els.previewPairs.textContent = estimatePairs(cells, inArea, v.radiusRings);
  }

  // estimatePairs mirrors the server: sum over origins of (gridDisk ∩ area) × profiles.
  // Exact when the work is bounded; otherwise a hex-disk approximation with a "~".
  function estimatePairs(cells, inArea, radiusRings) {
    var pf = Math.max(profiles.length, 1);
    var diskSize = 3 * radiusRings * (radiusRings + 1) + 1;
    if (cells.length * diskSize > 400000) {
      return "~" + (cells.length * diskSize * pf).toLocaleString();
    }
    var pairs = 0;
    for (var i = 0; i < cells.length; i++) {
      var neighbors = h3.gridDisk(cells[i], radiusRings);
      for (var j = 0; j < neighbors.length; j++) {
        if (inArea[neighbors[j]]) pairs++;
      }
    }
    return (pairs * pf).toLocaleString();
  }

  // ---- Apply ----------------------------------------------------------------
  function applyArea() {
    var v = controlValues();
    els.apply.disabled = true;
    els.apply.textContent = "Rebuilding…";

    fetch("/_config_/area", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(v),
    })
      .then(function (r) {
        return r.json().then(function (body) {
          if (!r.ok) throw new Error(body.error || "apply failed");
          return body;
        });
      })
      .then(function (summary) {
        toast(summary.cells.toLocaleString() + " cells · " + summary.pairs.toLocaleString() + " pairs seeded");
        // New working set: drop the old freshness overlay so it repaints clean.
        freshnessLayer.clearLayers();
        freshPolys = {};
        pollOnce();
      })
      .catch(function (err) {
        toast(err.message, true);
      })
      .finally(function () {
        els.apply.disabled = false;
        els.apply.textContent = "Apply & rebuild cache";
      });
  }

  // ---- Progress polling -----------------------------------------------------
  function pollOnce() {
    fetch("/_ops_/freshness").then(jsonOk).then(renderFreshness).catch(noop);
    fetch("/_ops_/cells").then(jsonOk).then(renderCells).catch(noop);
  }

  function renderFreshness(s) {
    var total = s.workingSet || 0;
    var fresh = Math.max(total - (s.debt || 0), 0);
    var pct = total > 0 ? (fresh / total) * 100 : 0;

    els.pct.textContent = pct.toFixed(0) + "%";
    els.frac.textContent = fresh.toLocaleString() + " / " + total.toLocaleString() + " fresh";
    els.barFill.style.width = pct.toFixed(1) + "%";

    els.mDebt.textContent = (s.debt || 0).toLocaleString();
    els.mOldest.textContent = fmtDuration(s.oldestAgeSeconds || 0);
    els.mThroughput.textContent =
      Math.round(s.achievedThroughput || 0).toLocaleString() +
      " / " +
      Math.round(s.requiredThroughput || 0).toLocaleString() +
      " /s";

    // Keeping up if we're sustaining the required rate, or already caught up.
    var keepingUp = (s.debt || 0) === 0 || (s.achievedThroughput || 0) >= (s.requiredThroughput || 0);
    els.mThroughputBox.className = "metric " + (keepingUp ? "keeping-up" : "behind");

    var debt = s.debt || 0;
    var rate = s.achievedThroughput || 0;
    if (debt === 0) {
      els.mEta.textContent = "caught up";
    } else if (rate > 0) {
      els.mEta.textContent = fmtDuration(debt / rate);
    } else {
      els.mEta.textContent = "—";
    }
  }

  function renderCells(resp) {
    var cells = resp.cells || [];
    var seen = Object.create(null);

    cells.forEach(function (c) {
      seen[c.cell] = true;
      var frac = c.total > 0 ? c.fresh / c.total : 0;
      var color = freshnessColor(frac);
      var existing = freshPolys[c.cell];
      if (existing) {
        existing.setStyle({ fillColor: color, color: color });
        return;
      }
      var poly = L.polygon(h3.cellToBoundary(c.cell), {
        color: color,
        weight: 1,
        opacity: 0.4,
        fillColor: color,
        fillOpacity: 0.4,
      });
      poly.bindTooltip(cellTooltip(c), { sticky: true });
      poly.addTo(freshnessLayer);
      freshPolys[c.cell] = poly;
    });

    // Drop polygons for cells no longer in the working set.
    Object.keys(freshPolys).forEach(function (cellId) {
      if (!seen[cellId]) {
        freshnessLayer.removeLayer(freshPolys[cellId]);
        delete freshPolys[cellId];
      }
    });
  }

  function cellTooltip(c) {
    return (
      "<b>" + c.fresh + " / " + c.total + " fresh</b><br>" +
      "oldest " + fmtDuration(c.oldestAgeSeconds) + "<br>" +
      "<code>" + c.cell + "</code>"
    );
  }

  // ---- Bootstrap ------------------------------------------------------------
  function loadInitialArea() {
    return fetch("/_config_/area")
      .then(jsonOk)
      .then(function (a) {
        profiles = a.profiles || [];
        maskResolution = a.maskResolution == null ? null : a.maskResolution;
        renderProfiles();
        setCenter({ lat: a.lat, lng: a.lng }, true);
        els.res.value = a.resolution;
        els.areaRings.value = a.areaRings;
        els.radiusRings.value = a.radiusRings;
        els.resVal.textContent = a.resolution;
        els.areaRingsVal.textContent = a.areaRings;
        els.radiusRingsVal.textContent = a.radiusRings;
        updateResHint();
        updateMaskWarn();
        map.setView([a.lat, a.lng], zoomForRes(a.resolution, a.areaRings));
        renderPreview();
      });
  }

  function renderProfiles() {
    els.profiles.innerHTML = "";
    profiles.forEach(function (p) {
      var span = document.createElement("span");
      span.className = "pill";
      span.textContent = p;
      els.profiles.appendChild(span);
    });
  }

  // ---- Helpers --------------------------------------------------------------
  function freshnessColor(frac) {
    // 0 → stale red, 0.5 → amber, 1 → fresh green (piecewise RGB lerp).
    var red = [242, 84, 91], amber = [255, 180, 84], green = [61, 220, 132];
    var a, b, t;
    if (frac < 0.5) { a = red; b = amber; t = frac / 0.5; }
    else { a = amber; b = green; t = (frac - 0.5) / 0.5; }
    var c = a.map(function (v, i) { return Math.round(v + (b[i] - v) * t); });
    return "rgb(" + c[0] + "," + c[1] + "," + c[2] + ")";
  }

  function zoomForRes(res, areaRings) {
    var z = res + 4 - Math.floor(areaRings / 4);
    return Math.max(3, Math.min(15, z));
  }

  function fmtDuration(sec) {
    sec = Math.round(sec);
    if (sec < 60) return sec + "s";
    var m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return m + "m " + s + "s";
    var h = Math.floor(m / 60);
    return h + "h " + (m % 60) + "m";
  }

  function fmtMeters(m) {
    return m >= 1000 ? (m / 1000).toFixed(m >= 10000 ? 0 : 1) + " km" : Math.round(m) + " m";
  }

  function jsonOk(r) {
    if (!r.ok) throw new Error("HTTP " + r.status);
    return r.json();
  }

  function noop() {}

  function toast(msg, isErr) {
    els.toast.textContent = msg;
    els.toast.className = "toast show" + (isErr ? " err" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { els.toast.className = "toast"; }, 3200);
  }

  // ---- Go -------------------------------------------------------------------
  initMap();
  bindControls();
  loadInitialArea()
    .catch(function (err) { toast("failed to load area: " + err.message, true); })
    .finally(function () {
      pollOnce();
      setInterval(pollOnce, 1500);
    });
})();
