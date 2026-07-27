/* Beeline operator console — multi-area, multi-layer.
 *
 * Areas live in the server's SQLite store, are disabled by default, and only refresh
 * once enabled. An area is a GeoJSON polygon plus precision layers (H3 resolutions);
 * every layer's cells derive from the polygon server-side. This console:
 *   1. Lists areas (/_config_/areas) with per-area enable/disable.
 *   2. Creates a one-layer area from an uploaded GeoJSON polygon (multi-layer areas
 *      are shaped via the API; layers render display-only here).
 *   3. Paints the selected area's cache-load progress from /_ops_/freshness?area=id
 *      and /_ops_/cells?area=id.
 *
 * h3-js (window.h3) does client-side polyfill/preview so it matches the server's
 * h3.PolygonToCells. Leaflet renders the map; tiles come from OSM.
 */
(function () {
  "use strict";

  var h3 = window.h3;
  var L = window.L;

  // HOVER_NEIGHBORS caps how many adjacent cells the hover overlay annotates with
  // cached duration/distance from the hovered cell (nearest rings first).
  var HOVER_NEIGHBORS = 100;

  // How long the area list waits for an answer, and how it backs off between
  // retries once it stops getting one.
  var AREAS_TIMEOUT_MS = 8000;
  var AREAS_RETRY_MIN_MS = 1000;
  var AREAS_RETRY_MAX_MS = 15000;

  // ---- State ----------------------------------------------------------------
  var areas = [];            // list rows from /_config_/areas
  var selectedId = null;     // currently selected area id
  var detail = null;         // selected area detail (incl. finest-layer cells[])
  var activeLayerRes = null; // resolution of the layer shown on the map (null = finest)
  var pendingGeoJSON = null; // parsed GeoJSON staged for create
  var map, areaLayer, freshnessLayer, previewLayer, hoverLayer;
  var areaPolys = {};        // cellId -> Leaflet polygon for the selected area
  var drawnCellSet = null;   // Set of the drawn layer's cell ids (hover neighbor clipping)
  var hoverTimer = null;     // debounce for the hover estimate probe
  var hoverAbort = null;     // in-flight hover fetch, cancelled on move-away
  var toastTimer;
  var areasError = null;     // last area-list failure, shown in place of the empty hint
  var areasRetry = null;     // pending area-list retry timer
  var areasRetryDelay = AREAS_RETRY_MIN_MS;

  function id(x) { return document.getElementById(x); }
  var els = {
    list: id("areas-list"), empty: id("areas-empty"),
    newBtn: id("new-area-btn"),
    createSection: id("create-section"),
    newName: id("new-name"), newRes: id("new-res"), newResVal: id("new-res-val"),
    newRadius: id("new-radius"), newRadiusVal: id("new-radius-val"),
    newStrategy: id("new-strategy"),
    newCore: id("new-core"), newCoreVal: id("new-core-val"), newCoreField: id("new-core-field"),
    newProvider: id("new-provider"),
    newTTL: id("new-ttl"),
    newTargetTTL: id("new-target-ttl"), newLease: id("new-lease"), newSweep: id("new-sweep"),
    newGeoJSON: id("new-geojson"), createPreview: id("create-preview"),
    previewCells: id("preview-cells"),
    createBtn: id("create-btn"), createCancel: id("create-cancel"),
    detailSection: id("detail-section"), detailName: id("detail-name"),
    detailBadge: id("detail-badge"), detailStrategy: id("detail-strategy"),
    detailProvider: id("detail-provider"),
    detailTTL: id("detail-ttl"), detailLayers: id("detail-layers"),
    detailTargetTTL: id("detail-target-ttl"), detailLease: id("detail-lease"), detailSweep: id("detail-sweep"),
    toggleEnable: id("toggle-enable"),
    replaceGeoJSON: id("replace-geojson"), deleteArea: id("delete-area"),
    editSettingsBtn: id("edit-settings-btn"), editSettings: id("edit-settings"),
    editName: id("edit-name"),
    editStrategy: id("edit-strategy"),
    editProvider: id("edit-provider"),
    editTTL: id("edit-ttl"), editReconvergeHint: id("edit-reconverge-hint"),
    editTargetTTL: id("edit-target-ttl"), editLease: id("edit-lease"), editSweep: id("edit-sweep"),
    editSave: id("edit-save"), editCancel: id("edit-cancel"),
    progressSection: id("progress-section"),
    pct: id("pct"), frac: id("frac"), barFill: id("bar-fill"),
    mDebt: id("m-debt"), mOldest: id("m-oldest"),
    mThroughput: id("m-throughput"), mThroughputBox: id("m-throughput-box"), mEta: id("m-eta"),
    toast: id("toast"),
  };

  // ---- Map ------------------------------------------------------------------
  function initMap() {
    // The initial view is a placeholder for an empty registry; boot recenters on
    // the first listed area once the list loads (centerOnFirstArea).
    map = L.map("map", { zoomControl: true }).setView([30.2672, -97.7431], 11);
    L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 19,
      attribution: "&copy; OpenStreetMap contributors",
    }).addTo(map);
    freshnessLayer = L.layerGroup().addTo(map);
    areaLayer = L.layerGroup().addTo(map);
    previewLayer = L.layerGroup().addTo(map);
    hoverLayer = L.layerGroup().addTo(map);
  }

  // Center the boot view on the first listed area, without selecting it. List rows
  // carry no geometry, so fetch the area's detail for its cell set.
  function centerOnFirstArea() {
    if (!areas.length) return;
    fetch("/_config_/areas/" + areas[0].id).then(jsonOk).then(function (a) {
      if ((a.cells || []).length) {
        map.fitBounds(cellsBounds(a.cells), { padding: [40, 40], maxZoom: 13 });
      }
    }).catch(noop);
  }

  // activeCells returns the cell ids of the layer currently shown on the map. The
  // finest layer's cells come precomputed in the detail response; any other layer
  // is polyfilled client-side from the area's GeoJSON (h3-js matches the server's
  // h3.PolygonToCells, so the drawn set is the seeded set).
  function activeCells() {
    if (!detail) return [];
    var finest = detail.layers.length ? detail.layers[0].resolution : null;
    if (activeLayerRes == null || activeLayerRes === finest) return detail.cells;
    if (!detail.geojson) return [];
    return polyfill(detail.geojson, activeLayerRes);
  }

  // Draw the active layer's cells. When enabled, freshness colors (from
  // renderCells) take over; here we draw the base outline so a disabled area
  // shows. Other layers' /_ops_/cells entries have no drawn polygon and are
  // simply not painted until their layer is selected in the layers table.
  function drawAreaCells() {
    areaLayer.clearLayers();
    clearHover();
    areaPolys = {};
    drawnCellSet = new Set();
    if (!detail) return;
    activeCells().forEach(function (c) {
      var poly = L.polygon(h3.cellToBoundary(c), {
        color: "#ffb454", weight: 1, opacity: 0.6,
        fillColor: "#ffb454", fillOpacity: detail.enabled ? 0 : 0.08,
      });
      poly.on("mouseover", function () { onCellHover(c); });
      poly.on("mouseout", clearHover);
      poly.addTo(areaLayer);
      areaPolys[c] = poly;
      drawnCellSet.add(c);
    });
  }

  // ---- Hover estimates --------------------------------------------------------
  // Hovering a cell annotates up to HOVER_NEIGHBORS adjacent cells (nearest rings
  // first, clipped to the drawn layer) with the cached duration/distance from the
  // hovered cell, via the /_ops_/pairs cache probe — a pure cache read keyed at the
  // drawn layer's own resolution, so it works on any layer. Cells whose pair is not
  // cached (yet) get a dim placeholder.
  function hoverNeighbors(cell) {
    // gridDisk returns ring order (nearest first); 6 rings = 127 candidates ≥ cap.
    var disk = [];
    try { disk = h3.gridDisk(cell, 6); } catch (err) { return []; }

    return disk.filter(function (c) {
      return c !== cell && drawnCellSet.has(c);
    }).slice(0, HOVER_NEIGHBORS);
  }

  function onCellHover(cell) {
    clearHover();
    if (!detail || selectedId == null) return;
    var neighbors = hoverNeighbors(cell);
    if (!neighbors.length) return;

    // Outline the origin immediately; labels land when the probe answers.
    var origin = areaPolys[cell];
    if (origin) {
      L.polygon(origin.getLatLngs(), {
        color: "#8ab4ff", weight: 2.5, opacity: 0.95, fill: false, interactive: false,
      }).addTo(hoverLayer);
    }

    hoverTimer = setTimeout(function () {
      hoverAbort = new AbortController();
      fetch("/_ops_/pairs", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ area: selectedId, origin: cell, dests: neighbors }),
        signal: hoverAbort.signal,
      }).then(jsonOrErr).then(function (resp) {
        drawHoverLabels(neighbors, resp.pairs || []);
      }).catch(noop); // aborted or transient — the overlay just stays label-less
    }, 150);
  }

  function drawHoverLabels(neighbors, pairs) {
    var byDest = {};
    pairs.forEach(function (p) { byDest[p.dest] = p; });
    neighbors.forEach(function (c) {
      var p = byDest[c];
      var html = p
        ? '<span class="t">' + fmtDuration(p.durationSec) + "</span><span class=\"d\">" + fmtDistance(p.distanceMeters) + "</span>"
        : '<span class="d">—</span>';
      var center = h3.cellToLatLng(c);
      L.marker([center[0], center[1]], {
        interactive: false,
        icon: L.divIcon({ className: "cell-label" + (p ? "" : " missing"), html: html, iconSize: [0, 0] }),
      }).addTo(hoverLayer);
    });
  }

  function clearHover() {
    clearTimeout(hoverTimer);
    hoverTimer = null;
    if (hoverAbort) { hoverAbort.abort(); hoverAbort = null; }
    if (hoverLayer) hoverLayer.clearLayers();
  }

  function fmtDistance(meters) {
    if (meters < 1000) return Math.round(meters) + " m";
    return (meters / 1000).toFixed(1) + " km";
  }

  // ---- Areas list -----------------------------------------------------------
  // refreshAreas re-lists the areas. Its failure is shown and retried rather
  // than swallowed, because the two states are not distinguishable to the eye:
  // a head that accepts the request and never answers (a starved connection
  // pool does exactly this) used to leave the sidebar with no rows, no "no
  // areas yet" hint and no error — identical to a healthy but empty registry —
  // since the boot fetch's rejection went to a no-op handler and nothing ever
  // re-listed. Resolves either way, so callers can keep chaining off it.
  function refreshAreas() {
    clearAreasRetry();

    return fetchWithTimeout("/_config_/areas", AREAS_TIMEOUT_MS)
      .then(jsonOk)
      .then(function (rows) {
        areas = rows || [];
        areasError = null;
        areasRetryDelay = AREAS_RETRY_MIN_MS;
        renderAreasList();
      })
      .catch(function (err) {
        // An abort is our own timeout firing, whose message ("The operation was
        // aborted") describes the client, not what went wrong.
        areasError = err.name === "AbortError" ? "no response in " + (AREAS_TIMEOUT_MS / 1000) + "s" : err.message;
        renderAreasList();
        scheduleAreasRetry();
      });
  }

  // scheduleAreasRetry re-lists on a backoff so the console heals by itself once
  // the head recovers — the poll loop only refreshes the *selected* area, so
  // without this a single failure is permanent until a manual reload.
  function scheduleAreasRetry() {
    clearAreasRetry();
    areasRetry = setTimeout(refreshAreas, areasRetryDelay);
    areasRetryDelay = Math.min(areasRetryDelay * 2, AREAS_RETRY_MAX_MS);
  }

  function clearAreasRetry() {
    if (areasRetry) clearTimeout(areasRetry);
    areasRetry = null;
  }

  // renderAreasError puts the failure where the areas would have been, with the
  // retry countdown already running and a button to skip the wait.
  function renderAreasError() {
    var box = document.createElement("div");
    box.className = "areas-error";

    var msg = document.createElement("span");
    msg.textContent = "Can't list areas: " + areasError;

    var retry = document.createElement("button");
    retry.className = "ghost";
    retry.type = "button";
    retry.textContent = "Retry";
    retry.addEventListener("click", refreshAreas);

    box.appendChild(msg);
    box.appendChild(retry);
    els.list.appendChild(box);
  }

  function renderAreasList() {
    els.list.innerHTML = "";
    // "No areas yet" is a claim about the registry, so it is only honest when
    // the registry actually answered; a failed list shows the error instead.
    els.empty.hidden = areas.length > 0 || areasError != null;
    if (areasError != null) renderAreasError();
    areas.forEach(function (a) {
      var row = document.createElement("div");
      row.className = "area-row" + (a.id === selectedId ? " selected" : "");

      var main = document.createElement("button");
      main.className = "area-main";
      var layers = a.layers || [];
      var meta = layers.length
        ? "res " + layers.map(function (l) { return l.resolution; }).join("/")
        : "no layers";
      main.innerHTML =
        '<span class="dot ' + (a.enabled ? "on" : "off") + '"></span>' +
        '<span class="area-name"></span>' +
        '<span class="area-meta">' + meta + "</span>";
      main.querySelector(".area-name").textContent = a.name;
      main.addEventListener("click", function () { selectArea(a.id); });

      var toggle = document.createElement("button");
      toggle.className = "area-toggle " + (a.enabled ? "on" : "off");
      toggle.textContent = a.enabled ? "On" : "Off";
      toggle.title = a.enabled ? "Disable" : "Enable";
      toggle.addEventListener("click", function (ev) {
        ev.stopPropagation();
        setEnabled(a.id, !a.enabled);
      });

      row.appendChild(main);
      row.appendChild(toggle);
      els.list.appendChild(row);
    });
  }

  function setEnabled(areaId, enabled) {
    postJSON("/_config_/areas/" + areaId + "/" + (enabled ? "enable" : "disable"), null)
      .then(function (updated) {
        toast(updated.name + (enabled ? " enabled" : " disabled"));
        refreshAreas();
        if (areaId === selectedId) applyDetail(updated);
      })
      .catch(function (err) { toast(err.message, true); });
  }

  // ---- Selection & detail ---------------------------------------------------
  function selectArea(areaId) {
    selectedId = areaId;
    activeLayerRes = null; // back to the finest layer on every (re)selection
    fetch("/_config_/areas/" + areaId).then(jsonOk).then(function (a) {
      applyDetail(a);
      if (detail.cells.length) {
        map.fitBounds(cellsBounds(detail.cells), { padding: [40, 40], maxZoom: 13 });
      }
      renderAreasList();
    }).catch(function (err) { toast(err.message, true); });
  }

  // finestCellCount is the selected area's finest-layer polyfill size — the count
  // the map actually draws (detail responses compute cellCount per layer).
  function finestCellCount(a) {
    return (a.layers && a.layers.length) ? (a.layers[0].cellCount || 0) : 0;
  }

  function applyDetail(a) {
    detail = a;
    detail.cells = a.cells || [];
    detail.layers = a.layers || [];

    // Keep the selected layer across refreshes; fall back to the finest if the
    // layer list changed underneath it.
    if (activeLayerRes != null && !detail.layers.some(function (l) { return l.resolution === activeLayerRes; })) {
      activeLayerRes = null;
    }

    closeEditSettings();
    els.detailSection.hidden = false;
    els.progressSection.hidden = !a.enabled;
    els.detailName.textContent = a.name;
    els.detailBadge.textContent = a.enabled ? "enabled" : "disabled";
    els.detailBadge.className = "badge " + (a.enabled ? "on" : "off");
    els.detailStrategy.textContent = a.warmStrategy || "eager";
    els.detailProvider.textContent = a.routingProvider || "haversine";
    els.detailTTL.textContent = (a.demandIdleTTL && a.demandIdleTTL !== "0s") ? a.demandIdleTTL : "no decay";
    els.detailTargetTTL.textContent = a.targetTTL || "—";
    els.detailLease.textContent = a.leaseDuration || "—";
    els.detailSweep.textContent = a.sweepInterval || "—";
    els.toggleEnable.textContent = a.enabled ? "Disable" : "Enable";
    renderLayersTable(detail.layers);

    drawAreaCells();
    if (!a.enabled) freshnessLayer.clearLayers();
  }

  // renderLayersTable paints the precision-layers table, finest first. Rows are
  // clickable: the selected row is the layer drawn (and freshness-painted) on the
  // map. Layer *config* stays display-only — reshape layers via the API — but each
  // row carries a cache-invalidation button (enabled areas only; a disabled area
  // has no pairs in the freshness index to re-enqueue).
  function renderLayersTable(layers) {
    var tbody = els.detailLayers.querySelector("tbody");
    tbody.innerHTML = "";
    var finest = layers.length ? layers[0].resolution : null;
    var shownRes = activeLayerRes == null ? finest : activeLayerRes;
    layers.forEach(function (l) {
      var tr = document.createElement("tr");
      if (l.resolution === shownRes) tr.className = "active";
      tr.title = "Show this layer on the map";
      [
        String(l.resolution),
        l.minDistanceMeters ? l.minDistanceMeters.toLocaleString() + " m" : "0",
        l.maxRadiusMeters ? l.maxRadiusMeters.toLocaleString() + " m" : "full mesh",
        l.coreRadiusMeters ? l.coreRadiusMeters.toLocaleString() + " m" : "—",
        (l.cellCount || 0).toLocaleString(),
      ].forEach(function (text) {
        var td = document.createElement("td");
        td.textContent = text;
        tr.appendChild(td);
      });

      var actions = document.createElement("td");
      if (detail && detail.enabled) {
        var btn = document.createElement("button");
        btn.className = "layer-action ghost danger";
        btn.type = "button";
        btn.textContent = "Invalidate";
        btn.title = "Re-enqueue this layer's cached pairs for refresh (cached values keep serving reads meanwhile)";
        btn.addEventListener("click", function (ev) {
          ev.stopPropagation(); // the row click selects the layer; the button must not
          invalidateLayer(l.resolution, btn);
        });
        actions.appendChild(btn);
      }
      tr.appendChild(actions);

      tr.addEventListener("click", function () { setActiveLayer(l.resolution); });
      tbody.appendChild(tr);
    });
  }

  // invalidateLayer re-enqueues one precision layer's cached pairs for refresh. The
  // estimates themselves survive — reads keep being answered from cache throughout —
  // so the only visible effect is a debt spike the pool then burns down, which is why
  // we poll immediately after.
  function invalidateLayer(res, btn) {
    if (!selectedId || !detail) return;
    if (!window.confirm(
      'Invalidate res ' + res + ' of "' + detail.name + '"?\n\n' +
      "Its cached pairs go to the front of the refresh queue. The cached values are " +
      "not dropped — reads keep being answered from them until they are recomputed."
    )) return;

    btn.disabled = true;
    postJSON("/_config_/areas/" + selectedId + "/invalidate?resolution=" + res, null)
      .then(function (r) {
        toast("res " + res + ": " + (r.invalidated || 0).toLocaleString() + " pairs queued for refresh");
        pollOnce();
      })
      .catch(function (err) { toast(err.message, true); })
      .finally(function () { btn.disabled = false; });
  }

  // setActiveLayer switches the map to another of the area's layers: redraw its
  // cells and repaint freshness right away (the poll would catch up anyway, but
  // this keeps the switch snappy).
  function setActiveLayer(res) {
    if (!detail) return;
    var finest = detail.layers.length ? detail.layers[0].resolution : null;
    activeLayerRes = res === finest ? null : res;
    renderLayersTable(detail.layers);
    freshnessLayer.clearLayers();
    drawAreaCells();
    pollOnce();
  }

  // ---- Edit settings --------------------------------------------------------
  // Populate the form from the selected area and PATCH every knob back. The
  // server overwrites all fields, so we always send the full set. Layers are
  // passed through unchanged (display-only here); a strategy change re-seeds
  // server-side and the read-only detail grid repaints from the response.
  function openEditSettings() {
    if (!detail) return;
    els.editName.value = detail.name;
    els.editStrategy.value = detail.warmStrategy || "eager";
    setSelectValue(els.editProvider, detail.routingProvider || "haversine");
    els.editTTL.value = (detail.demandIdleTTL && detail.demandIdleTTL !== "0s") ? detail.demandIdleTTL : "";
    els.editTargetTTL.value = detail.targetTTL || "";
    els.editLease.value = detail.leaseDuration || "";
    els.editSweep.value = detail.sweepInterval || "";

    els.editReconvergeHint.hidden = !detail.enabled;
    els.editSettings.hidden = false;
    els.editSettingsBtn.classList.add("active");
  }

  function closeEditSettings() {
    if (els.editSettings) els.editSettings.hidden = true;
    if (els.editSettingsBtn) els.editSettingsBtn.classList.remove("active");
  }

  function saveSettings() {
    if (selectedId == null || !detail) return;
    var name = els.editName.value.trim();
    if (!name) { toast("name is required", true); return; }
    var body = {
      name: name,
      warmStrategy: els.editStrategy.value,
      routingProvider: els.editProvider.value,
      layers: detail.layers, // unchanged pass-through; reshape layers via the API
    };
    var ttl = els.editTTL.value.trim();
    if (ttl) body.demandIdleTTL = ttl;
    if (els.editTargetTTL.value.trim()) body.targetTTL = els.editTargetTTL.value.trim();
    if (els.editLease.value.trim()) body.leaseDuration = els.editLease.value.trim();
    if (els.editSweep.value.trim()) body.sweepInterval = els.editSweep.value.trim();

    els.editSave.disabled = true;
    fetch("/_config_/areas/" + selectedId, {
      method: "PATCH",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    }).then(jsonOrErr).then(function (updated) {
      closeEditSettings();
      applyDetail(updated);
      refreshAreas();
      toast("settings saved");
    }).catch(function (err) { toast(err.message, true); })
      .finally(function () { els.editSave.disabled = false; });
  }

  // ---- Create ---------------------------------------------------------------
  function openCreate() {
    closeEditSettings();
    els.createSection.hidden = false;
    els.detailSection.hidden = true;
    els.progressSection.hidden = true;
    snapRadius(els.newRadius, els.newRadiusVal, els.newRes.value);
    selectedId = null;
    detail = null;
    areaLayer.clearLayers();
    freshnessLayer.clearLayers();
    clearHover();
    renderAreasList();
  }

  function closeCreate() {
    els.createSection.hidden = true;
    previewLayer.clearLayers();
    els.createPreview.hidden = true;
    pendingGeoJSON = null;
    els.newGeoJSON.value = "";
    els.newName.value = "";
  }

  function onGeoJSONChosen(input, onParsed) {
    var file = input.files && input.files[0];
    if (!file) { onParsed(null); return; }
    var reader = new FileReader();
    reader.onload = function () {
      try {
        onParsed(JSON.parse(reader.result));
      } catch (err) {
        toast("invalid GeoJSON: " + err.message, true);
        onParsed(null);
      }
    };
    reader.readAsText(file);
  }

  function previewCreateGeoJSON() {
    onGeoJSONChosen(els.newGeoJSON, function (gj) {
      pendingGeoJSON = gj;
      previewLayer.clearLayers();
      els.createPreview.hidden = true;
      if (!gj) return;
      var res = parseInt(els.newRes.value, 10);
      var cells = polyfill(gj, res);
      if (!cells.length) return;
      cells.forEach(function (c) {
        L.polygon(h3.cellToBoundary(c), {
          color: "#ffb454", weight: 1, opacity: 0.5,
          fillColor: "#ffb454", fillOpacity: 0.08, dashArray: "3,3", interactive: false,
        }).addTo(previewLayer);
      });
      els.previewCells.textContent = cells.length.toLocaleString();
      els.createPreview.hidden = false;
      map.fitBounds(cellsBounds(cells), { padding: [40, 40], maxZoom: 13 });
    });
  }

  function createArea() {
    var name = els.newName.value.trim();
    if (!name) { toast("name is required", true); return; }
    if (!pendingGeoJSON) { toast("a GeoJSON polygon is required", true); return; }
    var strategy = els.newStrategy.value;
    // The console creates one-layer areas; multi-layer areas are shaped via the API.
    var layer = {
      resolution: parseInt(els.newRes.value, 10),
      minDistanceMeters: 0,
      maxRadiusMeters: parseFloat(els.newRadius.value),
      coreRadiusMeters: strategy === "hybrid" ? parseFloat(els.newCore.value) : 0,
    };
    var body = {
      name: name,
      warmStrategy: strategy,
      geojson: pendingGeoJSON,
      layers: [layer],
    };
    if (els.newProvider.value) body.routingProvider = els.newProvider.value;
    var ttl = els.newTTL.value.trim();
    if (ttl) body.demandIdleTTL = ttl;
    if (els.newTargetTTL.value.trim()) body.targetTTL = els.newTargetTTL.value.trim();
    if (els.newLease.value.trim()) body.leaseDuration = els.newLease.value.trim();
    if (els.newSweep.value.trim()) body.sweepInterval = els.newSweep.value.trim();

    els.createBtn.disabled = true;
    postJSON("/_config_/areas", body)
      .then(function (created) {
        var n = finestCellCount(created);
        if (n === 0) {
          toast(created.name + " created, but 0 cells — the polygon is empty or too small to cover any hex at this resolution", true);
        } else {
          toast(created.name + " created (" + n.toLocaleString() + " cells, disabled)");
        }
        closeCreate();
        return refreshAreas().then(function () { selectArea(created.id); });
      })
      .catch(function (err) { toast(err.message, true); })
      .finally(function () { els.createBtn.disabled = false; });
  }

  // polyfill turns a GeoJSON geometry/feature/collection into H3 cells at res, using
  // h3-js in GeoJSON ([lng,lat]) mode. Best-effort preview; the server is authoritative.
  function polyfill(gj, res) {
    var out = [];
    eachPolygon(gj, function (coords) {
      try {
        out = out.concat(h3.polygonToCells(coords, res, true));
      } catch (err) { /* skip bad ring */ }
    });
    return out;
  }

  function eachPolygon(gj, fn) {
    if (!gj || !gj.type) return;
    if (gj.type === "FeatureCollection") {
      (gj.features || []).forEach(function (f) { eachPolygon(f, fn); });
    } else if (gj.type === "Feature") {
      eachPolygon(gj.geometry, fn);
    } else if (gj.type === "Polygon") {
      fn(gj.coordinates);
    } else if (gj.type === "MultiPolygon") {
      (gj.coordinates || []).forEach(function (p) { fn(p); });
    }
  }

  // ---- Replace geometry / delete --------------------------------------------
  function replaceGeoJSON() {
    onGeoJSONChosen(els.replaceGeoJSON, function (gj) {
      els.replaceGeoJSON.value = "";
      if (!gj || !selectedId) return;
      fetch("/_config_/areas/" + selectedId + "/geojson", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(gj),
      }).then(jsonOrErr).then(function (updated) {
        applyDetail(updated);
        var n = finestCellCount(updated);
        if (n === 0) {
          toast("geometry replaced, but 0 cells — the polygon is empty or too small to cover any hex at this resolution", true);
        } else {
          toast("geometry replaced (" + n.toLocaleString() + " cells)");
          map.fitBounds(cellsBounds(updated.cells), { padding: [40, 40], maxZoom: 13 });
        }
        refreshAreas();
      }).catch(function (err) { toast(err.message, true); });
    });
  }

  function deleteArea() {
    if (!selectedId || !detail) return;
    if (!window.confirm('Delete area "' + detail.name + '"? This cannot be undone.')) return;
    fetch("/_config_/areas/" + selectedId, { method: "DELETE" })
      .then(function (r) { if (!r.ok) throw new Error("HTTP " + r.status); })
      .then(function () {
        toast("area deleted");
        selectedId = null; detail = null;
        els.detailSection.hidden = true;
        els.progressSection.hidden = true;
        areaLayer.clearLayers(); freshnessLayer.clearLayers(); clearHover();
        refreshAreas();
      })
      .catch(function (err) { toast(err.message, true); });
  }

  // ---- Progress polling (selected, enabled area) ----------------------------
  function pollOnce() {
    if (!detail || !detail.enabled || selectedId == null) return;
    var q = "?area=" + selectedId;
    fetch("/_ops_/freshness" + q).then(jsonOk).then(renderFreshness).catch(noop);
    fetch("/_ops_/cells" + q).then(jsonOk).then(renderCells).catch(noop);
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
      Math.round(s.achievedThroughput || 0).toLocaleString() + " / " +
      Math.round(s.requiredThroughput || 0).toLocaleString() + " /s";

    var keepingUp = (s.debt || 0) === 0 || (s.achievedThroughput || 0) >= (s.requiredThroughput || 0);
    els.mThroughputBox.className = "metric " + (keepingUp ? "keeping-up" : "behind");

    var debt = s.debt || 0, rate = s.achievedThroughput || 0;
    els.mEta.textContent = debt === 0 ? "caught up" : (rate > 0 ? fmtDuration(debt / rate) : "—");
  }

  function renderCells(resp) {
    var cells = resp.cells || [];
    cells.forEach(function (c) {
      var frac = c.total > 0 ? c.fresh / c.total : 0;
      var color = freshnessColor(frac);
      var poly = areaPolys[c.cell];
      if (poly) {
        poly.setStyle({ color: color, fillColor: color, fillOpacity: 0.45 });
        poly.bindTooltip(cellTooltip(c), { sticky: true });
      }
    });
  }

  function cellTooltip(c) {
    return "<b>" + c.fresh + " / " + c.total + " fresh</b><br>oldest " +
      fmtDuration(c.oldestAgeSeconds) + "<br><code>" + c.cell + "</code>";
  }

  // ---- Helpers --------------------------------------------------------------
  function cellsBounds(cells) {
    var pts = [];
    cells.forEach(function (c) { pts = pts.concat(h3.cellToBoundary(c)); });
    return L.latLngBounds(pts);
  }

  function freshnessColor(frac) {
    var red = [242, 84, 91], amber = [255, 180, 84], green = [61, 220, 132];
    var a, b, t;
    if (frac < 0.5) { a = red; b = amber; t = frac / 0.5; }
    else { a = amber; b = green; t = (frac - 0.5) / 0.5; }
    var c = a.map(function (v, i) { return Math.round(v + (b[i] - v) * t); });
    return "rgb(" + c[0] + "," + c[1] + "," + c[2] + ")";
  }

  function fmtDuration(sec) {
    sec = Math.round(sec);
    if (sec < 60) return sec + "s";
    var m = Math.floor(sec / 60), s = sec % 60;
    if (m < 60) return m + "m " + s + "s";
    var h = Math.floor(m / 60);
    return h + "h " + (m % 60) + "m";
  }

  function postJSON(url, body) {
    var opts = { method: "POST", headers: { "Content-Type": "application/json" } };
    if (body != null) opts.body = JSON.stringify(body);
    return fetch(url, opts).then(jsonOrErr);
  }

  function jsonOrErr(r) {
    return r.json().then(function (body) {
      if (!r.ok) throw new Error(body.error || "HTTP " + r.status);
      return body;
    });
  }

  function jsonOk(r) {
    if (!r.ok) throw new Error("HTTP " + r.status);
    return r.json();
  }

  // fetchWithTimeout rejects with an AbortError once ms have passed without an
  // answer. fetch itself has no timeout: a server that accepts the connection
  // and then stalls — the shape of a starved connection pool — leaves the
  // promise pending forever, so no amount of error handling downstream can see it.
  function fetchWithTimeout(url, ms) {
    var ctl = new AbortController();
    var timer = setTimeout(function () { ctl.abort(); }, ms);

    return fetch(url, { signal: ctl.signal }).finally(function () { clearTimeout(timer); });
  }

  function noop() {}

  function toast(msg, isErr) {
    els.toast.textContent = msg;
    els.toast.className = "toast show" + (isErr ? " err" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { els.toast.className = "toast"; }, 3200);
  }

  // ---- Radius floor (Issue 1) ----------------------------------------------
  // At a given H3 resolution a bounded max-radius smaller than the distance to the
  // nearest neighbor cell yields zero neighbor rings, so every origin would pair only
  // with itself. minRadiusForRes returns that neighbor floor (meters) for res, sampled
  // near the current map view; snapRadius raises a radius slider up to it (and widens the
  // slider's max if the floor exceeds it). Zero (full mesh) is left untouched. The ~5%
  // margin keeps the snapped value above the server's own floor, which is measured from
  // the area's representative cell and so can differ slightly by location.
  var RADIUS_STEP = 500;

  function minRadiusForRes(res) {
    var c = map.getCenter();
    var cell = h3.latLngToCell(c.lat, c.lng, parseInt(res, 10));
    var here = h3.cellToLatLng(cell);
    var nearest = -1;
    h3.gridDisk(cell, 1).forEach(function (n) {
      if (n === cell) return;
      var d = h3.greatCircleDistance(here, h3.cellToLatLng(n), "m");
      if (nearest < 0 || d < nearest) nearest = d;
    });
    if (nearest < 0) return 0; // pentagon: no neighbor to measure
    return Math.ceil((nearest * 1.05) / RADIUS_STEP) * RADIUS_STEP;
  }

  function snapRadius(radiusEl, valEl, res) {
    var floor = minRadiusForRes(res);
    if (floor > Number(radiusEl.max)) radiusEl.max = String(floor);
    var v = parseFloat(radiusEl.value);
    if (v > 0 && v < floor) radiusEl.value = String(floor);
    valEl.textContent = radiusEl.value;
  }

  // ---- Bind & go ------------------------------------------------------------
  function bind() {
    els.newBtn.addEventListener("click", openCreate);
    els.createCancel.addEventListener("click", closeCreate);
    els.createBtn.addEventListener("click", createArea);
    els.newGeoJSON.addEventListener("change", previewCreateGeoJSON);
    els.newRes.addEventListener("input", function () {
      els.newResVal.textContent = els.newRes.value;
      snapRadius(els.newRadius, els.newRadiusVal, els.newRes.value);
      if (pendingGeoJSON) previewCreateGeoJSON();
    });
    els.newRadius.addEventListener("input", function () {
      snapRadius(els.newRadius, els.newRadiusVal, els.newRes.value);
    });
    els.newStrategy.addEventListener("change", function () {
      els.newCoreField.hidden = els.newStrategy.value !== "hybrid";
    });
    els.newCore.addEventListener("input", function () {
      els.newCoreVal.textContent = els.newCore.value;
    });
    els.toggleEnable.addEventListener("click", function () {
      if (selectedId != null) setEnabled(selectedId, !detail.enabled);
    });
    els.editSettingsBtn.addEventListener("click", function () {
      if (els.editSettings.hidden) openEditSettings(); else closeEditSettings();
    });
    els.editSave.addEventListener("click", saveSettings);
    els.editCancel.addEventListener("click", closeEditSettings);
    els.replaceGeoJSON.addEventListener("change", replaceGeoJSON);
    els.deleteArea.addEventListener("click", deleteArea);
  }

  // setSelectValue selects opt in sel, appending it if the option is absent (e.g. an
  // area references a provider no longer in config), so the current value stays visible.
  function setSelectValue(sel, opt) {
    if (!sel) return;
    var found = false;
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value === opt) { found = true; break; }
    }
    if (!found) sel.appendChild(new Option(opt + " (unconfigured)", opt));
    sel.value = opt;
  }

  // loadProviders fills the create/edit provider pickers from the live provider
  // registry (built-ins plus the database-backed operator entries), so the options
  // track whatever providers exist right now. Each entry is a full spec object;
  // the option value is the name, with the engine type as a hint for non-builtins.
  function loadProviders() {
    return fetch("/_config_/providers").then(jsonOrErr).then(function (providers) {
      [els.newProvider, els.editProvider].forEach(function (sel) {
        if (!sel) return;
        sel.innerHTML = "";
        (providers || []).forEach(function (p) {
          var label = p.builtin ? p.name : p.name + " (" + p.type + ")";
          sel.appendChild(new Option(label, p.name));
        });
      });
    }).catch(function () { /* non-fatal: the default is applied server-side */ });
  }

  initMap();
  bind();
  loadProviders();
  refreshAreas().then(centerOnFirstArea).finally(function () {
    pollOnce();
    setInterval(pollOnce, 1500);
  });
})();
