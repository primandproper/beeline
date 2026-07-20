/* Beeline operator console — multi-area.
 *
 * Areas live in the server's SQLite store, are disabled by default, and only refresh
 * once enabled. This console:
 *   1. Lists areas (/_config_/areas) with per-area enable/disable.
 *   2. Creates an area from an uploaded GeoJSON polygon (polyfilled server-side) or
 *      empty, to be filled in by hand.
 *   3. Refines the selected area hex-by-hex — click the map to toggle a cell
 *      (POST /_config_/areas/{id}/cells).
 *   4. Paints the selected area's cache-load progress from /_ops_/freshness?area=id
 *      and /_ops_/cells?area=id.
 *
 * h3-js (window.h3) does client-side polyfill/preview so it matches the server's
 * h3.PolygonToCells. Leaflet renders the map; tiles come from OSM.
 */
(function () {
  "use strict";

  var h3 = window.h3;
  var L = window.L;

  // ---- State ----------------------------------------------------------------
  var areas = [];            // list rows from /_config_/areas
  var selectedId = null;     // currently selected area id
  var detail = null;         // selected area detail (incl. cells[])
  var cellSet = null;        // Set of the selected area's cell ids (membership)
  var editMode = false;      // hex-toggle mode on the map
  var pendingGeoJSON = null; // parsed GeoJSON staged for create
  var excludeMode = false;   // subtract-region mode on the map
  var excludeVerts = [];     // [lat,lng] vertices of the region being traced
  var excludeTargets = [];   // area cells the traced region currently covers
  var map, areaLayer, freshnessLayer, previewLayer, excludeLayer;
  var areaPolys = {};        // cellId -> Leaflet polygon for the selected area
  var toastTimer;

  function id(x) { return document.getElementById(x); }
  var els = {
    list: id("areas-list"), empty: id("areas-empty"),
    newBtn: id("new-area-btn"),
    createSection: id("create-section"),
    newName: id("new-name"), newRes: id("new-res"), newResVal: id("new-res-val"),
    newRadius: id("new-radius"), newRadiusVal: id("new-radius-val"),
    newStrategy: id("new-strategy"),
    newCore: id("new-core"), newCoreVal: id("new-core-val"), newCoreField: id("new-core-field"),
    newGeoJSON: id("new-geojson"), createPreview: id("create-preview"),
    previewCells: id("preview-cells"),
    createBtn: id("create-btn"), createCancel: id("create-cancel"),
    detailSection: id("detail-section"), detailName: id("detail-name"),
    detailBadge: id("detail-badge"), detailRes: id("detail-res"),
    detailRadius: id("detail-radius"), detailStrategy: id("detail-strategy"), detailCells: id("detail-cells"),
    toggleEnable: id("toggle-enable"), editHexes: id("edit-hexes"),
    replaceGeoJSON: id("replace-geojson"), deleteArea: id("delete-area"),
    editHint: id("edit-hint"),
    excludeRegion: id("exclude-region"), excludeCtl: id("exclude-ctl"),
    excludeApply: id("exclude-apply"), excludeUndo: id("exclude-undo"),
    excludeCancel: id("exclude-cancel"),
    progressSection: id("progress-section"),
    pct: id("pct"), frac: id("frac"), barFill: id("bar-fill"),
    mDebt: id("m-debt"), mOldest: id("m-oldest"),
    mThroughput: id("m-throughput"), mThroughputBox: id("m-throughput-box"), mEta: id("m-eta"),
    toast: id("toast"),
  };

  // ---- Map ------------------------------------------------------------------
  function initMap() {
    map = L.map("map", { zoomControl: true }).setView([37.7749, -122.4194], 11);
    L.tileLayer("https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png", {
      maxZoom: 19,
      attribution: "&copy; OpenStreetMap contributors",
    }).addTo(map);
    freshnessLayer = L.layerGroup().addTo(map);
    areaLayer = L.layerGroup().addTo(map);
    previewLayer = L.layerGroup().addTo(map);
    excludeLayer = L.layerGroup().addTo(map);
    map.on("click", onMapClick);
  }

  // In edit mode, a click toggles the containing cell in/out of the selected area.
  // In exclude mode, a click drops a vertex of the region being traced.
  function onMapClick(e) {
    if (excludeMode) { excludeVerts.push([e.latlng.lat, e.latlng.lng]); drawExclude(); return; }
    if (!editMode || !detail) return;
    var cell = h3.latLngToCell(e.latlng.lat, e.latlng.lng, detail.resolution);
    var isIn = cellSet.has(cell);
    var body = isIn ? { remove: [cell] } : { add: [cell] };

    // Optimistic local update, then persist.
    if (isIn) cellSet.delete(cell); else cellSet.add(cell);
    detail.cells = Array.from(cellSet);
    drawAreaCells();

    postJSON("/_config_/areas/" + selectedId + "/cells", body)
      .then(function (updated) {
        applyDetail(updated);
        refreshAreas();
      })
      .catch(function (err) {
        // Revert on failure.
        if (isIn) cellSet.add(cell); else cellSet.delete(cell);
        detail.cells = Array.from(cellSet);
        drawAreaCells();
        toast(err.message, true);
      });
  }

  // Draw the selected area's cells. When enabled, freshness colors (from renderCells)
  // take over; here we draw the base outline so a disabled or just-edited area shows.
  function drawAreaCells() {
    areaLayer.clearLayers();
    areaPolys = {};
    if (!detail) return;
    detail.cells.forEach(function (c) {
      var poly = L.polygon(h3.cellToBoundary(c), {
        color: "#ffb454", weight: 1, opacity: 0.6,
        fillColor: "#ffb454", fillOpacity: detail.enabled ? 0 : 0.08,
      });
      poly.addTo(areaLayer);
      areaPolys[c] = poly;
    });
  }

  // ---- Areas list -----------------------------------------------------------
  function refreshAreas() {
    return fetch("/_config_/areas").then(jsonOk).then(function (rows) {
      areas = rows || [];
      renderAreasList();
    }).catch(noop);
  }

  function renderAreasList() {
    els.list.innerHTML = "";
    els.empty.hidden = areas.length > 0;
    areas.forEach(function (a) {
      var row = document.createElement("div");
      row.className = "area-row" + (a.id === selectedId ? " selected" : "");

      var main = document.createElement("button");
      main.className = "area-main";
      main.innerHTML =
        '<span class="dot ' + (a.enabled ? "on" : "off") + '"></span>' +
        '<span class="area-name"></span>' +
        '<span class="area-meta">' + a.cellCount.toLocaleString() + " cells</span>";
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
    setEditMode(false);
    exitExclude();
    fetch("/_config_/areas/" + areaId).then(jsonOk).then(function (a) {
      applyDetail(a);
      if (detail.cells.length) {
        map.fitBounds(cellsBounds(detail.cells), { padding: [40, 40], maxZoom: 13 });
      }
      renderAreasList();
    }).catch(function (err) { toast(err.message, true); });
  }

  function applyDetail(a) {
    detail = a;
    detail.cells = a.cells || [];
    cellSet = new Set(detail.cells);

    els.detailSection.hidden = false;
    els.progressSection.hidden = !a.enabled;
    els.detailName.textContent = a.name;
    els.detailBadge.textContent = a.enabled ? "enabled" : "disabled";
    els.detailBadge.className = "badge " + (a.enabled ? "on" : "off");
    els.detailRes.textContent = a.resolution;
    els.detailRadius.textContent = a.maxRadiusMeters ? a.maxRadiusMeters.toLocaleString() + " m" : "full mesh";
    els.detailStrategy.textContent = a.warmStrategy === "hybrid"
      ? "hybrid (" + (a.coreRadiusMeters || 0).toLocaleString() + " m core)"
      : (a.warmStrategy || "eager");
    els.detailCells.textContent = a.cellCount.toLocaleString();
    els.toggleEnable.textContent = a.enabled ? "Disable" : "Enable";

    drawAreaCells();
    if (!a.enabled) freshnessLayer.clearLayers();
  }

  function setEditMode(on) {
    if (on) exitExclude();
    editMode = on;
    els.editHexes.classList.toggle("active", on);
    els.editHint.hidden = !on;
    els.editHexes.textContent = on ? "Done editing" : "Edit hexes";
  }

  // ---- Subtract-region editor -----------------------------------------------
  // Trace a polygon on the map; the area cells its interior covers are previewed
  // in red and removed on Apply (POST …/cells {remove}). This is how operators
  // carve out water, private land, or anything else that should not be covered.
  function enterExclude() {
    if (!detail) return;
    setEditMode(false);
    excludeMode = true;
    excludeVerts = [];
    els.excludeRegion.classList.add("active");
    els.excludeCtl.hidden = false;
    drawExclude();
  }

  function exitExclude() {
    excludeMode = false;
    excludeVerts = [];
    excludeTargets = [];
    if (els.excludeRegion) els.excludeRegion.classList.remove("active");
    if (els.excludeCtl) els.excludeCtl.hidden = true;
    if (excludeLayer) excludeLayer.clearLayers();
  }

  // Redraw the in-progress region (vertices + edges) and, once it's a closed
  // triangle or larger, the area cells it would remove.
  function drawExclude() {
    excludeLayer.clearLayers();
    var pen = "#f2545b";
    excludeVerts.forEach(function (pt) {
      L.circleMarker(pt, { radius: 4, color: pen, weight: 2, fillColor: pen, fillOpacity: 0.9 }).addTo(excludeLayer);
    });
    if (excludeVerts.length >= 2) {
      L.polyline(excludeVerts, { color: pen, weight: 2, dashArray: "5,4" }).addTo(excludeLayer);
    }

    excludeTargets = [];
    if (excludeVerts.length >= 3 && detail) {
      L.polygon(excludeVerts, {
        color: pen, weight: 1.5, opacity: 0.8,
        fillColor: pen, fillOpacity: 0.06, dashArray: "5,4", interactive: false,
      }).addTo(excludeLayer);

      var ring = excludeVerts.map(function (pt) { return [pt[1], pt[0]]; }); // h3 wants [lng,lat]
      ring.push(ring[0]);
      var covered = [];
      try { covered = h3.polygonToCells([ring], detail.resolution, true); } catch (err) { covered = []; }
      excludeTargets = covered.filter(function (c) { return cellSet.has(c); });
      excludeTargets.forEach(function (c) {
        L.polygon(h3.cellToBoundary(c), {
          color: pen, weight: 1, fillColor: pen, fillOpacity: 0.5, interactive: false,
        }).addTo(excludeLayer);
      });
    }

    els.excludeApply.disabled = excludeTargets.length === 0;
    els.excludeApply.textContent = "Exclude " + excludeTargets.length + " cell" + (excludeTargets.length === 1 ? "" : "s");
  }

  function undoExcludeVertex() {
    excludeVerts.pop();
    drawExclude();
  }

  function applyExclude() {
    if (!excludeTargets.length || selectedId == null) return;
    var removed = excludeTargets.slice();

    // Optimistic local removal, then persist; exit the mode so the base outline shows.
    removed.forEach(function (c) { cellSet.delete(c); });
    detail.cells = Array.from(cellSet);
    exitExclude();
    drawAreaCells();

    postJSON("/_config_/areas/" + selectedId + "/cells", { remove: removed })
      .then(function (updated) {
        applyDetail(updated);
        refreshAreas();
        toast("excluded " + removed.length + " cell" + (removed.length === 1 ? "" : "s"));
      })
      .catch(function (err) {
        // Revert on failure.
        removed.forEach(function (c) { cellSet.add(c); });
        detail.cells = Array.from(cellSet);
        drawAreaCells();
        toast(err.message, true);
      });
  }

  // ---- Create ---------------------------------------------------------------
  function openCreate() {
    setEditMode(false);
    exitExclude();
    els.createSection.hidden = false;
    els.detailSection.hidden = true;
    els.progressSection.hidden = true;
    selectedId = null;
    detail = null;
    areaLayer.clearLayers();
    freshnessLayer.clearLayers();
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
    var strategy = els.newStrategy.value;
    var body = {
      name: name,
      resolution: parseInt(els.newRes.value, 10),
      maxRadiusMeters: parseFloat(els.newRadius.value),
      warmStrategy: strategy,
    };
    if (strategy === "hybrid") body.coreRadiusMeters = parseFloat(els.newCore.value);
    if (pendingGeoJSON) body.geojson = pendingGeoJSON;

    els.createBtn.disabled = true;
    postJSON("/_config_/areas", body)
      .then(function (created) {
        if (created.cellCount === 0) {
          toast(created.name + " created, but 0 cells — the polygon is empty or too small to cover any hex at this resolution", true);
        } else {
          toast(created.name + " created (" + created.cellCount.toLocaleString() + " cells, disabled)");
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
        if (updated.cellCount === 0) {
          toast("geometry replaced, but 0 cells — the polygon is empty or too small to cover any hex at this resolution", true);
        } else {
          toast("geometry replaced (" + updated.cellCount.toLocaleString() + " cells)");
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
        areaLayer.clearLayers(); freshnessLayer.clearLayers();
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

  function noop() {}

  function toast(msg, isErr) {
    els.toast.textContent = msg;
    els.toast.className = "toast show" + (isErr ? " err" : "");
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { els.toast.className = "toast"; }, 3200);
  }

  // ---- Bind & go ------------------------------------------------------------
  function bind() {
    els.newBtn.addEventListener("click", openCreate);
    els.createCancel.addEventListener("click", closeCreate);
    els.createBtn.addEventListener("click", createArea);
    els.newGeoJSON.addEventListener("change", previewCreateGeoJSON);
    els.newRes.addEventListener("input", function () {
      els.newResVal.textContent = els.newRes.value;
      if (pendingGeoJSON) previewCreateGeoJSON();
    });
    els.newRadius.addEventListener("input", function () {
      els.newRadiusVal.textContent = els.newRadius.value;
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
    els.editHexes.addEventListener("click", function () { setEditMode(!editMode); });
    els.excludeRegion.addEventListener("click", function () { if (excludeMode) exitExclude(); else enterExclude(); });
    els.excludeApply.addEventListener("click", applyExclude);
    els.excludeUndo.addEventListener("click", undoExcludeVertex);
    els.excludeCancel.addEventListener("click", exitExclude);
    els.replaceGeoJSON.addEventListener("change", replaceGeoJSON);
    els.deleteArea.addEventListener("click", deleteArea);
  }

  initMap();
  bind();
  refreshAreas().finally(function () {
    pollOnce();
    setInterval(pollOnce, 1500);
  });
})();
