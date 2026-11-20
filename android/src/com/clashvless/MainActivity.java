package com.clashvless;

import android.app.Activity;
import android.app.AlertDialog;
import android.app.Dialog;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Intent;
import android.content.res.ColorStateList;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.graphics.Color;
import android.graphics.Typeface;
import android.graphics.drawable.BitmapDrawable;
import android.graphics.drawable.ColorDrawable;
import android.graphics.drawable.GradientDrawable;
import android.graphics.drawable.RippleDrawable;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.text.Editable;
import android.text.InputType;
import android.text.TextUtils;
import android.text.TextWatcher;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.view.Window;
import android.view.WindowManager;
import android.widget.Button;
import android.widget.CheckBox;
import android.widget.EditText;
import android.widget.FrameLayout;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.Switch;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.Arrays;
import java.util.HashSet;
import java.util.List;
import java.util.Set;
import java.util.function.Consumer;
import java.util.function.IntConsumer;

/** Native Android dashboard over the same daemon protocol as desktop. */
public class MainActivity extends Activity {
    static final String ACTION_CONNECT = "com.clashvless.CONNECT";
    private static final String[] TABS = {"Settings", "Overview", "Log"};
    private static final String[] ICONS = {"settings", "overview", "activity"};
    private static final int CONFIG = 0, STATUS = 1, LOG = 2;
    private static int BG = Color.rgb(15, 19, 25), CARD = Color.rgb(24, 30, 38);
    private static int TEXT = Color.rgb(230, 235, 241), MUTED = Color.rgb(145, 157, 171);
    private static int ACCENT = Color.rgb(104, 213, 189), DANGER = Color.rgb(244, 126, 139);
    private static int CONTROL = Color.rgb(31, 39, 49), BORDER = Color.rgb(43, 53, 65), BLUE = Color.rgb(105, 169, 255);
    private static final int WARNING = Color.rgb(238, 190, 105);

    private int tab = STATUS, configSection;
    private boolean switchUpdating;
    private TextView connectionLabel;
    private Switch vpnSwitch;
    private final Button[] tabBtns = new Button[TABS.length];
    private FrameLayout contentFrame;
    private LinearLayout body;
    private ScrollView scroll;
    private TextView logView;
    private final Set<String> logLevels = new HashSet<>(Arrays.asList("INFO", "WARN", "ERROR"));
    private String logModule = "", logQuery = "";
    private String selectedSlotName;
    private boolean slotsMenuExpanded;
    private String nodeQuery = "", nodeSort = "Recommended";
    private boolean nodesOnline, liveRenderQueued, sectionsOpen;
    private long typedAt;
    private boolean touching;
    private volatile boolean logQueued;
    private final java.util.Map<String, Boolean> collapsedSubs = new java.util.HashMap<>();
    private int renderedView = -1;

    @Override
    protected void onCreate(Bundle saved) {
        super.onCreate(saved);
        loadBranding();
        if (Build.VERSION.SDK_INT >= 33) requestPermissions(new String[]{"android.permission.POST_NOTIFICATIONS"}, 0);
        getWindow().setStatusBarColor(BG);
        getWindow().setNavigationBarColor(BG);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(BG);
        root.setPadding(dp(14), dp(8), dp(14), 0);

        LinearLayout top = new LinearLayout(this);
        top.setGravity(Gravity.CENTER_VERTICAL);
        ImageView logo = new ImageView(this);
        logo.setImageBitmap(loadBitmap("logo"));
        top.addView(logo, new LinearLayout.LayoutParams(dp(38), dp(38)));
        TextView headline = text("clashvless", 21, TEXT, true);
        headline.setPadding(dp(10), 0, dp(10), 0);
        top.addView(headline, new LinearLayout.LayoutParams(0, -2, 1));
        LinearLayout connection = new LinearLayout(this);
        connection.setOrientation(LinearLayout.VERTICAL);
        connection.setGravity(Gravity.CENTER_HORIZONTAL);
        connection.setMinimumWidth(dp(88));
        connectionLabel = text("Disconnected", 12, MUTED, true);
        connectionLabel.setSingleLine(true);
        connectionLabel.setGravity(Gravity.CENTER);
        connection.addView(connectionLabel, new LinearLayout.LayoutParams(dp(88), -2));
        vpnSwitch = new Switch(this);
        vpnSwitch.setShowText(false);
        vpnSwitch.setThumbTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{BG, MUTED}));
        vpnSwitch.setTrackTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{ACCENT, Color.rgb(51, 66, 88)}));
        vpnSwitch.setOnCheckedChangeListener((view, on) -> {
            if (!switchUpdating) toggleVpn();
        });
        connection.addView(vpnSwitch, new LinearLayout.LayoutParams(dp(78), dp(44)));
        top.addView(connection);
        root.addView(top);

        contentFrame = new FrameLayout(this);
        scroll = new ScrollView(this);
        scroll.setFillViewport(true);
        body = new LinearLayout(this);
        body.setOrientation(LinearLayout.VERTICAL);
        body.setPadding(0, dp(6), 0, dp(20));
        scroll.addView(body, new ScrollView.LayoutParams(-1, -2));
        contentFrame.addView(scroll, new FrameLayout.LayoutParams(-1, -1));
        root.addView(contentFrame, new LinearLayout.LayoutParams(-1, 0, 1));

        LinearLayout tabs = new LinearLayout(this);
        tabs.setGravity(Gravity.CENTER);
        tabs.setPadding(0, dp(3), 0, dp(5));
        for (int i = 0; i < TABS.length; i++) {
            final int selected = i;
            tabBtns[i] = btn(TABS[i], false, () -> navigate(selected));
            tabBtns[i].setTextSize(11);
            setFooterIcon(tabBtns[i], ICONS[i], MUTED);
            tabs.addView(tabBtns[i], new LinearLayout.LayoutParams(0, dp(56), 1));
        }
        root.addView(tabs);
        setContentView(root);
        Ctl.start(this);
        handleConnectIntent(getIntent());
    }

    @Override
    protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        handleConnectIntent(intent);
    }

    private void handleConnectIntent(Intent intent) {
        if (intent != null && ACTION_CONNECT.equals(intent.getAction()) && !VpnSvc.running) {
            contentFrame.post(this::toggleVpn);
            intent.setAction(null);
        }
    }

    @Override
    protected void onStart() {
        super.onStart();
        Core.uiVisible = true;
        Ctl.wake();
        Ctl.listener = stateChanged -> runOnUiThread(() -> {
            if (stateChanged && tab == CONFIG) render();
            else if (tab == STATUS && !touching) render();
            else if (liveView()) queueLiveRender();
            else renderHead();
        });
        render();
    }

    @Override
    protected void onStop() {
        Core.uiVisible = false;
        Ctl.listener = null;
        Core.setListener(null);
        if (!isChangingConfigurations()) new Thread(Core::stopIdle).start();
        super.onStop();
    }

    @Override
    public void onConfigurationChanged(android.content.res.Configuration config) {
        super.onConfigurationChanged(config);
        render();
    }

    /** A rebuild between press and release drops the tap, so live redraws wait for the finger to lift. */
    @Override
    public boolean dispatchTouchEvent(android.view.MotionEvent event) {
        int action = event.getActionMasked();
        if (action == android.view.MotionEvent.ACTION_DOWN) touching = true;
        else if (action == android.view.MotionEvent.ACTION_UP || action == android.view.MotionEvent.ACTION_CANCEL) touching = false;
        return super.dispatchTouchEvent(event);
    }

    /** Overview, Exit routes and Providers show live state, so status updates redraw them too. */
    private boolean liveView() {
        return tab == STATUS || (tab == CONFIG && (configSection == 2 || configSection == 3));
    }

    private void queueLiveRender() {
        renderHead();
        if (liveRenderQueued) return;
        liveRenderQueued = true;
        body.postDelayed(() -> {
            liveRenderQueued = false;
            if (!liveView()) return;
            // Not under an open category list, or mid-typing: a rebuild resets the keyboard's composing text.
            if (sectionsOpen || touching || android.os.SystemClock.uptimeMillis() - typedAt < 3000) queueLiveRender();
            else render();
        }, 1500);
    }

    private void navigate(int selected) {
        tab = selected;
        render();
    }

    private void render() {
        renderHead();
        for (int i = 0; i < TABS.length; i++) styleTab(tabBtns[i], i == tab);
        int view = tab * 100 + (tab == CONFIG ? configSection : 0), keepY = scroll.getScrollY();
        boolean sameView = view == renderedView;
        renderedView = view;
        sectionsOpen = false;
        View focused = getCurrentFocus();
        Object focusTag = focused instanceof EditText ? focused.getTag() : null;
        body.removeAllViews();
        if (sameView && tab != LOG) scroll.post(() -> scroll.scrollTo(0, keepY));
        if (tab != LOG) Core.setListener(null);
        JSONObject st = Ctl.state;
        if (tab != LOG && (!Ctl.connected || st == null)) {
            body.addView(card("Connecting", "Starting the local engine and reading its state..."));
            return;
        }
        if (tab == STATUS) renderStatus(st, body);
        else if (tab == LOG) renderLog();
        else renderConfig(st);
        View again = focusTag == null ? null : body.findViewWithTag(focusTag);
        if (again instanceof EditText) {
            again.requestFocus();
            ((EditText) again).setSelection(((EditText) again).length());
        }
    }

    private void renderHead() {
        JSONObject s = Ctl.status;
        boolean up = VpnSvc.running && s != null && s.optBoolean("TunUp"), online = up && s.optInt("Tier") > 0;
        connectionLabel.setText(!VpnSvc.running ? "Disconnected" : online ? "Connected" : up ? "No route" : "Connecting");
        connectionLabel.setTextColor(online ? ACCENT : MUTED);
        switchUpdating = true;
        vpnSwitch.setChecked(VpnSvc.running);
        switchUpdating = false;
    }

    static String line(JSONObject s) {
        int tier = s.optInt("Tier");
        long ms = s.optLong("Egress") / 1_000_000;
        String main = s.optString("Main");
        if (main.isEmpty()) main = "main";
        if (tier == 0) return s.optString("Err", "Waiting for a route");
        if (tier == 1) return "T1 direct -> " + main + " | " + ms + " ms";
        return "T" + tier + " " + s.optString("Entry") + " -> " + main + " | " + ms + " ms";
    }

    private void renderStatus(JSONObject st, LinearLayout target) {
        JSONObject s = Ctl.status == null ? new JSONObject() : Ctl.status;
        JSONArray slots = st.optJSONArray("slots");
        if (!anyMain(st) && slots != null && slots.length() > 0) target.addView(setupCard(st, slots.optJSONObject(0).optString("name")), spaced());
        target.addView(connectionCard(st, s), spaced());
        target.addView(endpointsCard(st, s), spaced());
        target.addView(poolsCard(s), spaced());
        target.addView(btn("Recheck every route", false, () -> Ctl.cmd("kick")));
    }

    private View setupCard(JSONObject st, String firstSlot) {
        JSONArray subs = st.optJSONArray("subs");
        boolean haveSubs = subs != null && subs.length() > 0;
        LinearLayout setup = card("Finish setup", "Two steps before traffic can be routed");
        setup.addView(setupStep(haveSubs, "Add a subscription", "Provides the entry nodes used as hops (T2/T3)",
                btn("Add subscription", false, this::addSub)), topMargin(8));
        setup.addView(setupStep(false, "Add a final VLESS exit", "The server your traffic finally leaves from",
                btn("Add final exits", true, () -> addExits(firstSlot))), topMargin(10));
        return setup;
    }

    private View setupStep(boolean done, String title, String hint, Button action) {
        LinearLayout step = new LinearLayout(this);
        step.setOrientation(LinearLayout.VERTICAL);
        LinearLayout head = new LinearLayout(this);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(done ? tintChip("Done", ACCENT) : tintChip("To do", WARNING));
        TextView name = text(title, 13, TEXT, true);
        name.setPadding(dp(8), 0, 0, 0);
        head.addView(name, new LinearLayout.LayoutParams(0, -2, 1));
        step.addView(head);
        step.addView(text(hint, 12, MUTED, false));
        if (!done) step.addView(action, topMargin(4));
        return step;
    }

    private View connectionCard(JSONObject st, JSONObject s) {
        boolean vpn = VpnSvc.running, online = s.optInt("Tier") > 0, tunErr = !s.optString("TunErr").isEmpty();
        String headline = "Disconnected", hint = "No exit works yet. Retrying automatically.";
        boolean good = false;
        if (vpn && tunErr) { headline = "Not protected"; hint = "The VPN could not start, so traffic goes out directly."; }
        else if (vpn && !s.optBoolean("TunUp")) { headline = "Connecting"; hint = "Starting the VPN; traffic goes out directly until it is up."; }
        else if (vpn && online) { headline = "Connected"; hint = "All traffic goes through the route below."; good = true; }
        else if (vpn) { headline = "No route"; hint = "The VPN is up, but no exit works yet. Retrying automatically."; }
        else if (online) { headline = "Proxy ready"; hint = "Apps set to a local SOCKS5 address are routed. Turn the VPN on to route everything."; good = true; }
        else if (s.optBoolean("HopOnly")) hint = "No final exit works; only the first-hop port is served.";
        LinearLayout card = card(text(headline, 17, good ? ACCENT : TEXT, true), null);
        card.addView(text(hint, 12, MUTED, false));

        String route = "No working route yet";
        int routeColor = MUTED;
        if (online) {
            route = s.optString("Entry").isEmpty() ? "This device -> " + s.optString("Main") : "This device -> " + s.optString("Entry") + " -> " + s.optString("Main");
            routeColor = TEXT;
        } else if (s.optBoolean("HopOnly")) {
            route = "This device -> " + s.optString("Main") + " (first hop only)";
            routeColor = WARNING;
        } else if (!s.optString("Err").isEmpty()) {
            route = s.optString("Err");
            routeColor = DANGER;
        }
        LinearLayout routeRow = new LinearLayout(this);
        TextView label = text("ROUTE", 11, MUTED, true);
        label.setPadding(0, dp(3), dp(8), 0);
        routeRow.addView(label);
        TextView routeText = text(route, 13, routeColor, true);
        routeText.setMaxLines(2);
        routeText.setEllipsize(TextUtils.TruncateAt.END);
        routeRow.addView(routeText, new LinearLayout.LayoutParams(0, -2, 1));
        card.addView(routeRow, topMargin(8));

        int serving = 0, enabled = 0;
        JSONArray slots = st.optJSONArray("slots"), live = s.optJSONArray("Slots");
        for (int i = 0; slots != null && i < slots.length(); i++) if (slots.optJSONObject(i).optBoolean("enabled")) enabled++;
        for (int i = 0; live != null && i < live.length(); i++) if (live.optJSONObject(i).optInt("Tier") > 0) serving++;
        int tier = s.optInt("Tier");
        LinearLayout metrics = new LinearLayout(this);
        addMetric(metrics, "Latency", online ? s.optLong("Egress") / 1_000_000 + " ms" : "-");
        addMetric(metrics, "Route type", online ? "T" + tier + " " + TIER_NAMES[Math.min(tier, 3)] : "-");
        addMetric(metrics, "Slots serving", serving + " / " + enabled);
        card.addView(metrics, topMargin(6));
        if (!s.optString("Note").isEmpty()) card.addView(text(s.optString("Note"), 12, MUTED, false), topMargin(6));
        if (tunErr) {
            LinearLayout alert = toned(DANGER);
            alert.addView(text("VPN isn't running: traffic is NOT going through the tunnel. Retrying automatically.", 13, TEXT, true));
            alert.addView(text(s.optString("TunErr"), 12, MUTED, false));
            card.addView(alert, topMargin(8));
        }
        return card;
    }

    private View endpointsCard(JSONObject st, JSONObject s) {
        LinearLayout card = card("Local endpoints", "SOCKS5 addresses for your applications");
        JSONArray slots = st.optJSONArray("slots");
        String tun = tunSlot(st), host = connectHost(st);
        int shown = 0, disabled = 0;
        for (int i = 0; slots != null && i < slots.length(); i++) {
            JSONObject sl = slots.optJSONObject(i);
            if (!sl.optBoolean("enabled")) { disabled++; continue; }
            String name = sl.optString("name"), address = "socks5://" + host + ":" + sl.optInt("port");
            int hop = hopPort(st, i);
            LinearLayout box = innerBox();
            LinearLayout head = new LinearLayout(this);
            head.setGravity(Gravity.CENTER_VERTICAL);
            TextView title = oneLine(name, 14, TEXT, true);
            head.addView(title, new LinearLayout.LayoutParams(0, -2, 1));
            if ("proxy".equals(sl.optString("type"))) {
                TextView proxyChip = neutralChip("HTTP/SOCKS");
                LinearLayout.LayoutParams lp = new LinearLayout.LayoutParams(-2, -2);
                lp.setMargins(0, 0, dp(6), 0);
                head.addView(proxyChip, lp);
            }
            if (name.equals(tun)) head.addView(tintChip("VPN", ACCENT));
            box.addView(head);
            box.addView(text(address + (hop > 0 ? "  |  first hop :" + hop : ""), 12, MUTED, false));
            Object[] state = liveState(true, liveSlot(s, name));
            LinearLayout stateRow = new LinearLayout(this);
            stateRow.setGravity(Gravity.CENTER_VERTICAL);
            stateRow.addView((View) state[0]);
            TextView detail = oneLine((String) state[1], 12, (int) state[2], false);
            detail.setPadding(dp(8), 0, 0, 0);
            stateRow.addView(detail, new LinearLayout.LayoutParams(0, -2, 1));
            box.addView(stateRow, topMargin(4));
            List<View> buttons = new ArrayList<>();
            buttons.add(btn("Copy", false, () -> copy(address)));
            if (hop > 0) buttons.add(btn("Copy hop", false, () -> copy("socks5://" + host + ":" + hop)));
            box.addView(equalRow(buttons.toArray(new View[0])), topMargin(6));
            if (shown++ > 0) box.setLayoutParams(topMargin(8));
            card.addView(box);
        }
        if (shown == 0) card.addView(text("No enabled slots. Enable one in Settings > Exit routes.", 12, MUTED, false), topMargin(6));
        if (disabled > 0) card.addView(text(disabled + " disabled slot(s) not shown", 12, MUTED, false), topMargin(6));
        return card;
    }

    private View poolsCard(JSONObject s) {
        LinearLayout card = card("Entry pools", "Hop nodes from your subscriptions");
        Set<String> inUse = entriesInUse(s);
        JSONArray[] pools = pools(s);
        card.addView(poolBox("Country exits", 2, pools[0], inUse));
        card.addView(poolBox("Bypass nodes", 3, pools[1], inUse));
        JSONObject sweep = s.optJSONObject("Sweep");
        if (sweep != null && !sweep.optString("Node").isEmpty()) {
            card.addView(text("Speed test " + (sweep.optInt("Done") + 1) + " / " + sweep.optInt("Total") + " | testing " + sweep.optString("Node"), 12, MUTED, false), topMargin(6));
        }
        return card;
    }

    private View poolBox(String name, int tier, JSONArray probes, Set<String> inUse) {
        int up = 0, measured = 0;
        double flaky = 0;
        JSONObject fastest = null;
        String using = "";
        for (int i = 0; i < probes.length(); i++) {
            JSONObject probe = probes.optJSONObject(i);
            if (probe.optBoolean("OK")) {
                up++;
                if (fastest == null || probe.optLong("Latency") < fastest.optLong("Latency")) fastest = probe;
            }
            if (probe.optInt("Samples") > 0) { measured++; flaky += probe.optDouble("Flakiness"); }
            if (using.isEmpty() && inUse.contains(probe.optString("Name"))) using = probe.optString("Name");
        }
        LinearLayout box = innerBox();
        LinearLayout head = new LinearLayout(this);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(text(name, 13, TEXT, true));
        TextView tierChip = neutralChip("T" + tier);
        LinearLayout.LayoutParams chipParams = new LinearLayout.LayoutParams(-2, -2);
        chipParams.setMargins(dp(8), 0, 0, 0);
        head.addView(tierChip, chipParams);
        head.addView(new View(this), new LinearLayout.LayoutParams(0, 1, 1));
        head.addView(text(up + " / " + probes.length() + " online", 13, TEXT, true));
        box.addView(head);
        if (probes.length() == 0) {
            box.addView(text("No nodes in your subscriptions", 12, MUTED, false));
            return box;
        }
        float ratio = (float) up / probes.length();
        box.addView(meter(ratio, up == 0 ? DANGER : ratio < 0.5f ? WARNING : ACCENT), topMargin(6));
        box.addView(text(measured > 0 ? String.format(java.util.Locale.US, "Avg failure rate %.0f%% | last hour", flaky / measured) : "Stability: measuring...", 12, MUTED, false), topMargin(4));
        if (fastest != null) box.addView(oneLine("Fastest: " + fastest.optString("Name") + " | " + fastest.optLong("Latency") / 1_000_000 + " ms", 12, MUTED, false));
        if (!using.isEmpty()) box.addView(oneLine("In use: " + using, 12, ACCENT, true));
        return box;
    }

    /** A thin bar filled to ratio. */
    private View meter(float ratio, int color) {
        LinearLayout track = new LinearLayout(this);
        track.setBackground(round(CONTROL, 3));
        View fill = new View(this);
        fill.setBackground(round(color, 3));
        track.addView(fill, new LinearLayout.LayoutParams(0, dp(6), Math.max(0, Math.min(1, ratio))));
        track.addView(new View(this), new LinearLayout.LayoutParams(0, dp(6), 1 - Math.max(0, Math.min(1, ratio))));
        return track;
    }

    private void renderSubs(JSONObject st) {
        JSONArray subs = st.optJSONArray("subs");
        int count = subs == null ? 0 : subs.length();
        boolean combined = st.optBoolean("auto_select");
        LinearLayout providers = card("Providers", count == 0 ? "Remnawave subscriptions that supply entry nodes"
                : combined ? "Nodes from all providers are combined" : "Only the active provider's nodes are used");
        Button refresh = btn("Refresh all", false, () -> {
            if (Ctl.cmd("refetch")) fetching("refetching all subscriptions");
        });
        refresh.setEnabled(count > 0);
        if (count == 0) refresh.setAlpha(0.4f);
        providers.addView(spacer(8));
        providers.addView(equalRow(btn("Add subscription", true, this::addSub), refresh));
        if (count > 1) providers.addView(toggle("Use nodes from all providers", combined, on -> Ctl.patch("auto_select", on)));
        if (count == 0) providers.addView(text("No subscriptions yet. Add a subscription URL to get entry nodes for hopped routes (T2/T3).", 12, MUTED, false));
        Set<String> online = new HashSet<>();
        JSONObject s = Ctl.status;
        for (JSONArray pool : pools(s)) {
            for (int i = 0; i < pool.length(); i++) if (pool.optJSONObject(i).optBoolean("OK")) online.add(pool.optJSONObject(i).optString("Name"));
        }
        for (int i = 0; i < count; i++) providers.addView(providerRow(st, subs.optJSONObject(i), i, online));
        body.addView(providers, spaced());
        body.addView(nodesCard(st));
    }

    private View providerRow(JSONObject st, JSONObject sub, int index, Set<String> online) {
        boolean active = st.optBoolean("auto_select") || st.optInt("active_sub") == index;
        JSONArray nodes = sub.optJSONArray("nodes");
        int total = nodes == null ? 0 : nodes.length(), bypass = 0, up = 0;
        for (int i = 0; i < total; i++) {
            JSONObject node = nodes.optJSONObject(i);
            if (node.optBoolean("whitelist")) bypass++;
            if (online.contains(node.optString("name"))) up++;
        }
        String error = sub.optString("fetch_err");
        View tag = !error.isEmpty() ? tintChip("Fetch failed", DANGER) : active ? tintChip("Active", ACCENT) : neutralChip("Not used");
        String facts = total + " nodes";
        if (bypass > 0) facts += " (" + (total - bypass) + " country, " + bypass + " bypass)";
        if (active && total > 0) facts += " | " + up + " online";
        facts += " | " + ago(sub.optString("last_fetch"));

        LinearLayout box = innerBox();
        LinearLayout head = new LinearLayout(this);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(oneLine(sub.optString("name"), 14, TEXT, true), new LinearLayout.LayoutParams(0, -2, 1));
        head.addView(tag);
        box.addView(head);
        box.addView(text(facts, 12, MUTED, false));
        if (!error.isEmpty()) box.addView(text("Last fetch failed: " + error, 12, DANGER, false));

        LinearLayout actions = new LinearLayout(this);
        actions.setGravity(Gravity.CENTER_VERTICAL);
        if (!active) {
            actions.addView(btn("Use this", false, () -> {
                try { Ctl.patch(new JSONObject().put("auto_select", false).put("active_sub", index)); } catch (Exception e) { failed(e); }
            }));
        }
        actions.addView(new View(this), new LinearLayout.LayoutParams(0, 1, 1));
        String name = sub.optString("name"), url = sub.optString("url");
        actions.addView(icon(R.drawable.clashvless_qr, "Export nodes as VLESS links", total > 0, () -> {
            try { Exports.call(this, new JSONObject().put("nodes", nodes), response -> showShare("Export | " + name, response)); } catch (Exception e) { failed(e); }
        }));
        actions.addView(icon(R.drawable.clashvless_delete, "Remove subscription", true, () -> confirm("Remove " + name + " and its cached nodes?", () -> {
            // By URL: another client may have removed an earlier subscription meanwhile.
            JSONArray current = Ctl.state == null ? null : Ctl.state.optJSONArray("subs");
            for (int i = 0; current != null && i < current.length(); i++) {
                if (url.equals(current.optJSONObject(i).optString("url"))) { Ctl.cmd("rmsub", "index", i); return; }
            }
            toast("This subscription was already removed");
        })));
        box.addView(actions);
        return box;
    }

    private static final String[] SORTS = {"Recommended", "Lowest latency", "Name", "Provider order"};

    private static final class NodeGroup {
        String key, name;
        boolean probed;
        final List<List<JSONObject>> pools = Arrays.asList(new ArrayList<>(), new ArrayList<>());
        final List<List<JSONObject>> unchecked = Arrays.asList(new ArrayList<>(), new ArrayList<>());
        // Probe or placeholder -> this provider's node: names can repeat across providers.
        final java.util.Map<JSONObject, JSONObject> nodes = new java.util.IdentityHashMap<>();
    }

    private static final class NodeEntry {
        final View row;
        final String search;
        final boolean ok;

        NodeEntry(View row, String search, boolean ok) {
            this.row = row;
            this.search = search;
            this.ok = ok;
        }
    }

    /** One group per provider; probes exist only for nodes the engine checks, the rest go to "Other nodes". */
    private static List<NodeGroup> nodeGroups(JSONObject st, JSONObject s) {
        java.util.Map<String, JSONObject> probes = new java.util.HashMap<>();
        JSONArray[] pools = pools(s);
        for (int pool = 0; pool < 2; pool++) {
            for (int i = 0; i < pools[pool].length(); i++) probes.put(pool + pools[pool].optJSONObject(i).optString("Name"), pools[pool].optJSONObject(i));
        }
        Set<String> used = new HashSet<>();
        List<NodeGroup> groups = new ArrayList<>();
        JSONArray subs = st.optJSONArray("subs");
        for (int i = 0; subs != null && i < subs.length(); i++) {
            JSONObject sub = subs.optJSONObject(i);
            NodeGroup group = new NodeGroup();
            group.key = sub.optString("url");
            group.name = sub.optString("name");
            group.probed = st.optBoolean("auto_select") || st.optInt("active_sub") == i;
            JSONArray nodes = sub.optJSONArray("nodes");
            for (int n = 0; nodes != null && n < nodes.length(); n++) {
                JSONObject node = nodes.optJSONObject(n);
                int pool = node.optBoolean("whitelist") ? 1 : 0;
                String key = pool + node.optString("name");
                JSONObject probe = probes.get(key);
                if (probe != null && group.probed) {
                    group.pools.get(pool).add(probe);
                    group.nodes.put(probe, node);
                    used.add(key);
                } else {
                    try {
                        JSONObject placeholder = new JSONObject().put("Name", node.optString("name")).put("Server", node.optString("server"));
                        group.unchecked.get(pool).add(placeholder);
                        group.nodes.put(placeholder, node);
                    } catch (Exception ignored) { }
                }
            }
            groups.add(group);
        }
        NodeGroup other = new NodeGroup();
        other.key = "";
        other.name = "Other nodes";
        other.probed = true;
        for (int pool = 0; pool < 2; pool++) {
            for (int i = 0; i < pools[pool].length(); i++) {
                JSONObject probe = pools[pool].optJSONObject(i);
                if (!used.contains(pool + probe.optString("Name"))) other.pools.get(pool).add(probe);
            }
        }
        if (!other.pools.get(0).isEmpty() || !other.pools.get(1).isEmpty()) groups.add(other);
        return groups;
    }

    private boolean groupCollapsed(NodeGroup group) {
        Boolean collapsed = collapsedSubs.get(group.key);
        return collapsed != null ? collapsed : !group.probed;
    }

    private View nodesCard(JSONObject st) {
        JSONObject s = Ctl.status;
        LinearLayout card = card("Entry nodes", "Grouped by provider. Pin a node to stop automatic selection.");
        card.addView(spacer(8));
        EditText query = new EditText(this);
        query.setSingleLine(true);
        query.setHint("Search by name or server");
        query.setHintTextColor(MUTED);
        query.setTextColor(TEXT);
        query.setTextSize(13);
        query.setPadding(dp(10), 0, dp(10), 0);
        query.setBackground(round(CONTROL, 8));
        query.setText(nodeQuery);
        query.setTag("nodeQuery");
        card.addView(query, new LinearLayout.LayoutParams(-1, dp(42)));
        CheckBox onlineOnly = check("Online only", nodesOnline);
        Button sort = btn("Sort: " + nodeSort, false, () -> choose("Sort nodes", SORTS, which -> {
            nodeSort = SORTS[which];
            render();
        }));
        card.addView(row(onlineOnly, sort));

        String pin = st.optString("pin_entry");
        if (!pin.isEmpty()) {
            LinearLayout banner = toned(WARNING);
            LinearLayout texts = new LinearLayout(this);
            texts.setOrientation(LinearLayout.VERTICAL);
            texts.addView(oneLine("Pinned: " + pin, 13, TEXT, true));
            texts.addView(text("Automatic entry selection is off for every slot", 11, MUTED, false));
            LinearLayout line = new LinearLayout(this);
            line.setGravity(Gravity.CENTER_VERTICAL);
            line.addView(texts, new LinearLayout.LayoutParams(0, -2, 1));
            line.addView(btn("Unpin", false, () -> Ctl.patch("pin_entry", "")));
            banner.addView(line);
            card.addView(banner, topMargin(8));
        }
        JSONObject sweep = s == null ? null : s.optJSONObject("Sweep");
        if (sweep != null && !sweep.optString("Node").isEmpty()) {
            card.addView(oneLine("Speed test " + (sweep.optInt("Done") + 1) + " / " + sweep.optInt("Total") + " | testing " + sweep.optString("Node"), 12, MUTED, false), topMargin(6));
        }

        Set<String> inUse = entriesInUse(s);
        String sweeping = sweep == null ? "" : sweep.optString("Node");
        List<NodeEntry> entries = new ArrayList<>();
        List<java.util.function.IntSupplier> refreshers = new ArrayList<>();
        List<NodeGroup> groups = s == null ? new ArrayList<>() : nodeGroups(st, s);
        for (NodeGroup group : groups) {
            LinearLayout box = innerBox();
            LinearLayout groupBody = new LinearLayout(this);
            groupBody.setOrientation(LinearLayout.VERTICAL);
            int first = entries.size(), total = 0, up = 0;
            String[] titles = {"COUNTRY EXITS | T2", "BYPASS NODES | T3"};
            for (int pool = 0; pool < 2; pool++) {
                List<JSONObject> probes = sortProbes(group.pools.get(pool), nodeSort);
                List<JSONObject> unchecked = "Name".equals(nodeSort) ? sortProbes(group.unchecked.get(pool), "Name") : group.unchecked.get(pool);
                if (probes.isEmpty() && unchecked.isEmpty()) continue;
                TextView heading = text(titles[pool], 10, MUTED, true);
                heading.setPadding(0, dp(8), 0, dp(2));
                groupBody.addView(heading);
                for (JSONObject probe : probes) {
                    total++;
                    if (probe.optBoolean("OK")) up++;
                    View row = nodeRow(st, probe, group.nodes.get(probe), inUse.contains(probe.optString("Name")), sweeping, "");
                    entries.add(new NodeEntry(row, (probe.optString("Name") + " " + probe.optString("Server")).toLowerCase(java.util.Locale.ROOT), probe.optBoolean("OK")));
                    groupBody.addView(row);
                }
                for (JSONObject probe : unchecked) {
                    total++;
                    View row = nodeRow(st, probe, group.nodes.get(probe), false, sweeping, group.probed ? "Not checked yet" : "Provider not in use");
                    entries.add(new NodeEntry(row, (probe.optString("Name") + " " + probe.optString("Server")).toLowerCase(java.util.Locale.ROOT), false));
                    groupBody.addView(row);
                }
            }
            TextView empty = text(total == 0 ? "This provider has no nodes yet" : "No nodes match", 12, MUTED, false);
            groupBody.addView(empty);

            ImageView chevron = new ImageView(this);
            chevron.setImageResource(R.drawable.clashvless_expand);
            chevron.setColorFilter(MUTED);
            TextView counts = text("", 11, MUTED, false);
            LinearLayout header = new LinearLayout(this);
            header.setGravity(Gravity.CENTER_VERTICAL);
            header.setMinimumHeight(dp(40));
            header.addView(chevron, new LinearLayout.LayoutParams(dp(20), dp(20)));
            LinearLayout titles2 = new LinearLayout(this);
            titles2.setOrientation(LinearLayout.VERTICAL);
            titles2.setPadding(dp(8), 0, dp(8), 0);
            titles2.addView(oneLine(group.name, 14, TEXT, true));
            titles2.addView(counts);
            header.addView(titles2, new LinearLayout.LayoutParams(0, -2, 1));
            header.addView(group.key.isEmpty() ? neutralChip("Not in a provider") : group.probed ? tintChip("Active", ACCENT) : neutralChip("Not used"));
            header.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), null, round(Color.WHITE, 6)));
            Runnable showFold = () -> {
                boolean folded = groupCollapsed(group);
                groupBody.setVisibility(folded ? View.GONE : View.VISIBLE);
                chevron.setRotation(folded ? -90 : 0);
                header.setContentDescription((folded ? "Expand " : "Collapse ") + group.name);
            };
            showFold.run();
            header.setOnClickListener(v -> {
                collapsedSubs.put(group.key, !groupCollapsed(group));
                showFold.run();
            });
            box.addView(header);
            box.addView(groupBody);
            card.addView(box);

            List<NodeEntry> mine = new ArrayList<>(entries.subList(first, entries.size()));
            final int groupTotal = total, groupUp = up;
            refreshers.add(() -> {
                int shown = 0;
                for (NodeEntry entry : mine) if (entry.row.getVisibility() == View.VISIBLE) shown++;
                String text = groupTotal == 0 ? "no nodes" : !group.probed ? groupTotal + " nodes | not checked" : groupUp + " of " + groupTotal + " online";
                if (!nodeQuery.trim().isEmpty() || nodesOnline) text = shown + " match | " + text;
                counts.setText(text);
                empty.setVisibility(groupTotal == 0 || shown == 0 ? View.VISIBLE : View.GONE);
                return shown;
            });
        }
        if (groups.isEmpty()) card.addView(text(s == null ? "Waiting for the engine's first check..." : "No entry nodes yet. Add a subscription above.", 12, MUTED, false), topMargin(8));
        TextView summary = text("", 11, MUTED, false);
        summary.setPadding(0, dp(8), 0, 0);
        card.addView(summary);

        Runnable apply = () -> {
            String q = nodeQuery.trim().toLowerCase(java.util.Locale.ROOT);
            for (NodeEntry entry : entries) {
                boolean hidden = (!q.isEmpty() && !entry.search.contains(q)) || (nodesOnline && !entry.ok);
                entry.row.setVisibility(hidden ? View.GONE : View.VISIBLE);
            }
            int shown = 0;
            for (java.util.function.IntSupplier refresh : refreshers) shown += refresh.getAsInt();
            summary.setText("Showing " + shown + " of " + entries.size());
        };
        query.addTextChangedListener(new TextWatcher() {
            public void beforeTextChanged(CharSequence text, int start, int count, int after) { }
            public void onTextChanged(CharSequence text, int start, int before, int count) { }
            public void afterTextChanged(Editable text) {
                nodeQuery = text.toString();
                typedAt = android.os.SystemClock.uptimeMillis();
                apply.run();
            }
        });
        onlineOnly.setOnCheckedChangeListener((view, on) -> { nodesOnline = on; apply.run(); });
        apply.run();
        return card;
    }

    /** A checked node, or with a note an unchecked one (no stats, no pin). */
    private View nodeRow(JSONObject st, JSONObject probe, JSONObject source, boolean inUse, String sweeping, String note) {
        String name = probe.optString("Name");
        boolean ok = probe.optBoolean("OK"), pinned = name.equals(st.optString("pin_entry"));
        LinearLayout entry = new LinearLayout(this);
        entry.setOrientation(LinearLayout.VERTICAL);
        View divider = new View(this);
        divider.setBackgroundColor(BORDER);
        entry.addView(divider, new LinearLayout.LayoutParams(-1, 1));

        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(7), 0, dp(7));
        View dot = new View(this);
        GradientDrawable circle = new GradientDrawable();
        circle.setShape(GradientDrawable.OVAL);
        circle.setColor(!note.isEmpty() ? MUTED : ok ? ACCENT : DANGER);
        dot.setBackground(circle);
        LinearLayout.LayoutParams dotParams = new LinearLayout.LayoutParams(dp(8), dp(8));
        dotParams.setMargins(dp(2), 0, dp(10), 0);
        row.addView(dot, dotParams);

        LinearLayout info = new LinearLayout(this);
        info.setOrientation(LinearLayout.VERTICAL);
        info.addView(oneLine(name, 13, ok || !note.isEmpty() ? TEXT : MUTED, inUse || pinned));
        FlowLayout tags = flow();
        if (inUse) tags.addView(tintChip("In use", ACCENT));
        if (pinned) tags.addView(tintChip("Pinned", WARNING));
        if (name.equals(sweeping)) tags.addView(tintChip("Testing speed...", BLUE));
        if (!note.isEmpty()) tags.addView(neutralChip(note));
        else if (ok) tags.addView(neutralChip(probe.optLong("Latency") / 1_000_000 + " ms"));
        else tags.addView(tintChip("Offline", DANGER));
        if (probe.optInt("SpeedN") > 0) {
            double speed = probe.optDouble("Speed");
            tags.addView(speed < 1 ? tintChip("<1 Mbps", DANGER) : neutralChip(Math.round(speed) + " Mbps"));
        }
        if (probe.optInt("Samples") > 0) {
            double flake = probe.optDouble("Flakiness");
            String label = Math.round(flake) + "% fails";
            tags.addView(flake > 30 ? tintChip(label, DANGER) : flake > 10 ? tintChip(label, WARNING) : neutralChip(label));
        }
        info.addView(tags, topMargin(3));
        info.addView(oneLine(probe.optString("Server"), 11, MUTED, false));
        row.addView(info, new LinearLayout.LayoutParams(0, -2, 1));

        row.addView(icon(R.drawable.clashvless_qr, "Share as VLESS link", true, () -> {
            JSONObject node = source != null ? source : findNode(name);
            if (node == null) { toast("This node is no longer in your subscriptions"); return; }
            try { Exports.call(this, new JSONObject().put("nodes", new JSONArray().put(node)), response -> showShare("Share entry node", response)); } catch (Exception e) { failed(e); }
        }));
        if (note.isEmpty() || pinned) {
            Button pin = btn(pinned ? "Unpin" : "Pin", false, () -> Ctl.patch("pin_entry", pinned ? "" : name));
            pin.setMinWidth(0);
            pin.setMinimumWidth(0);
            row.addView(pin);
        }
        entry.addView(row);
        return entry;
    }

    private static JSONObject findNode(String name) {
        JSONArray subs = Ctl.state == null ? null : Ctl.state.optJSONArray("subs");
        for (int i = 0; subs != null && i < subs.length(); i++) {
            JSONArray nodes = subs.optJSONObject(i).optJSONArray("nodes");
            for (int n = 0; nodes != null && n < nodes.length(); n++) {
                if (name.equals(nodes.optJSONObject(n).optString("name"))) return nodes.optJSONObject(n);
            }
        }
        return null;
    }

    /** Orders a copy of a pool for display; Recommended mirrors the engine: online, steadier, faster, then lower ping. */
    static List<JSONObject> sortProbes(List<JSONObject> probes, String mode) {
        List<JSONObject> sorted = new ArrayList<>(probes);
        java.util.Comparator<JSONObject> offline = java.util.Comparator.comparingInt(p -> p.optBoolean("OK") ? 0 : 1);
        java.util.Comparator<JSONObject> latency = java.util.Comparator.comparingLong(p -> p.optLong("Latency"));
        switch (mode) {
            case "Provider order":
                return sorted;
            case "Name":
                sorted.sort(java.util.Comparator.comparing(p -> p.optString("Name").toLowerCase(java.util.Locale.ROOT)));
                return sorted;
            case "Lowest latency":
                sorted.sort(offline.thenComparing(latency));
                return sorted;
        }
        sorted.sort(offline
                .thenComparingInt(p -> p.optInt("Samples") > 0 ? (int) (p.optDouble("Flakiness") / 10) : 3)
                .thenComparingInt(p -> p.optInt("SpeedN") > 0 ? 0 : 1)
                .thenComparing(java.util.Comparator.comparingDouble((JSONObject p) -> p.optInt("SpeedN") > 0 ? p.optDouble("Speed") : 0).reversed())
                .thenComparing(latency));
        return sorted;
    }

    /** Entry nodes some slot is serving through right now. */
    private static Set<String> entriesInUse(JSONObject s) {
        Set<String> used = new HashSet<>();
        JSONArray slots = s == null ? null : s.optJSONArray("Slots");
        for (int i = 0; slots != null && i < slots.length(); i++) {
            JSONObject slot = slots.optJSONObject(i);
            if (slot.optBoolean("HopOnly")) used.add(slot.optString("Main"));
            else if (slot.optInt("Tier") > 1 && !slot.optString("Entry").isEmpty()) used.add(slot.optString("Entry"));
        }
        return used;
    }

    private static JSONArray[] pools(JSONObject s) {
        JSONArray country = s == null ? null : s.optJSONArray("Country"), bypass = s == null ? null : s.optJSONArray("Bypass");
        return new JSONArray[]{country == null ? new JSONArray() : country, bypass == null ? new JSONArray() : bypass};
    }

    static String ago(String fetched) {
        long time = 0;
        try {
            java.text.SimpleDateFormat format = new java.text.SimpleDateFormat("yyyy-MM-dd'T'HH:mm:ssXXX", java.util.Locale.ROOT);
            time = format.parse(fetched.replaceFirst("\\.\\d+", "")).getTime();
        } catch (Exception ignored) { }
        if (time <= 0) return "never fetched";
        long minutes = (System.currentTimeMillis() - time) / 60_000;
        if (minutes < 1) return "updated just now";
        if (minutes < 60) return "updated " + minutes + " min ago";
        if (minutes < 48 * 60) return "updated " + minutes / 60 + " h ago";
        return "updated " + new java.text.SimpleDateFormat("dd MMM yyyy", java.util.Locale.US).format(new java.util.Date(time));
    }

    private void renderMain(JSONObject st) {
        body.addView(btn("Add failover slot", true, this::addSlot), spaced());
        JSONArray slots = st.optJSONArray("slots");
        String vpnSlot = tunSlot(st);
        String tunSlot = vpnSlot.isEmpty() && slots != null && slots.length() > 0 ? slots.optJSONObject(0).optString("name") : vpnSlot;
        JSONObject s = Ctl.status;

        if (slots == null || slots.length() == 0) {
            body.addView(card("", "No failover slots configured. Tap \"Add failover slot\" to create one."));
            body.addView(card("Routing compatibility", "Vision exits are direct-only. Plain VLESS and HTTP/SOCKS5 exits can use any hop; REALITY exits require a non-Vision hop."));
            return;
        }

        if (!slotsMenuExpanded) {
            selectedSlotName = tunSlot;
        }
        int selectedIndex = -1;
        JSONObject selectedSlot = null;
        for (int i = 0; i < slots.length(); i++) {
            JSONObject sl = slots.optJSONObject(i);
            if (sl.optString("name").equals(selectedSlotName)) {
                selectedIndex = i;
                selectedSlot = sl;
                break;
            }
        }
        if (selectedSlot == null) {
            for (int i = 0; i < slots.length(); i++) {
                JSONObject sl = slots.optJSONObject(i);
                if (sl.optString("name").equals(tunSlot)) {
                    selectedIndex = i;
                    selectedSlot = sl;
                    selectedSlotName = tunSlot;
                    break;
                }
            }
        }
        if (selectedSlot == null && slots.length() > 0) {
            selectedIndex = 0;
            selectedSlot = slots.optJSONObject(0);
            selectedSlotName = selectedSlot.optString("name");
        }

        LinearLayout slotTabsMenu = new LinearLayout(this);
        slotTabsMenu.setOrientation(LinearLayout.VERTICAL);
        slotTabsMenu.setPadding(dp(12), dp(10), dp(12), dp(10));
        slotTabsMenu.setBackground(round(CARD, 10));
        LinearLayout.LayoutParams menuParams = new LinearLayout.LayoutParams(-1, -2);
        menuParams.setMargins(0, 0, 0, dp(8));
        slotTabsMenu.setLayoutParams(menuParams);

        LinearLayout menuHeader = new LinearLayout(this);
        menuHeader.setOrientation(LinearLayout.HORIZONTAL);
        menuHeader.setGravity(Gravity.CENTER_VERTICAL);
        menuHeader.setPadding(0, 0, 0, dp(6));

        LinearLayout titleCol = new LinearLayout(this);
        titleCol.setOrientation(LinearLayout.VERTICAL);
        titleCol.addView(text("FAILOVER SLOTS", 12, ACCENT, true));
        titleCol.addView(text(slotsMenuExpanded ? "All slots shown" : "Showing VPN slot only", 11, MUTED, false));
        menuHeader.addView(titleCol, new LinearLayout.LayoutParams(0, -2, 1));

        Button expandBtn = btn(slotsMenuExpanded ? "Collapse" : "Expand", false, () -> {
            slotsMenuExpanded = !slotsMenuExpanded;
            if (!slotsMenuExpanded) {
                selectedSlotName = tunSlot;
            }
            render();
        });
        expandBtn.setContentDescription(slotsMenuExpanded ? "Collapse slot tabs" : "Expand slot tabs");
        menuHeader.addView(expandBtn);
        slotTabsMenu.addView(menuHeader);

        for (int i = 0; i < slots.length(); i++) {
            JSONObject sl = slots.optJSONObject(i);
            String name = sl.optString("name");
            boolean isTun = name.equals(tunSlot);
            if (!slotsMenuExpanded && !isTun) {
                continue;
            }
            boolean isSelected = name.equals(selectedSlotName);

            LinearLayout tabItem = new LinearLayout(this);
            tabItem.setOrientation(LinearLayout.HORIZONTAL);
            tabItem.setGravity(Gravity.CENTER_VERTICAL);
            tabItem.setPadding(dp(12), dp(8), dp(12), dp(8));
            tabItem.setMinimumHeight(dp(44));

            GradientDrawable tabBg = round(isSelected ? Color.rgb(27, 37, 44) : Color.TRANSPARENT, 8);
            if (isSelected) {
                tabBg.setStroke(dp(1), ACCENT);
            } else {
                tabBg.setStroke(dp(1), Color.rgb(43, 53, 65));
            }
            tabItem.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), tabBg, round(Color.WHITE, 8)));

            LinearLayout tabTexts = new LinearLayout(this);
            tabTexts.setOrientation(LinearLayout.VERTICAL);
            tabTexts.addView(oneLine(name, 14, isSelected ? ACCENT : TEXT, true));
            String portDesc = "socks5://" + connectHost(st) + ":" + sl.optInt("port");
            tabTexts.addView(text(portDesc, 11, MUTED, false));
            tabItem.addView(tabTexts, new LinearLayout.LayoutParams(0, -2, 1));

            if ("proxy".equals(sl.optString("type"))) {
                TextView proxyBadge = neutralChip("HTTP/SOCKS");
                LinearLayout.LayoutParams pParams = new LinearLayout.LayoutParams(-2, -2);
                pParams.setMargins(0, 0, dp(6), 0);
                tabItem.addView(proxyBadge, pParams);
            }
            if (isTun) {
                TextView vpnBadge = text("VPN", 10, ACCENT, true);
                vpnBadge.setPadding(dp(6), dp(2), dp(6), dp(2));
                vpnBadge.setBackground(round(Color.rgb(19, 44, 40), 4));
                tabItem.addView(vpnBadge);
            }

            final String tabSlotName = name;
            tabItem.setOnClickListener(v -> {
                selectedSlotName = tabSlotName;
                render();
            });

            LinearLayout.LayoutParams itemParams = new LinearLayout.LayoutParams(-1, -2);
            itemParams.setMargins(0, dp(2), 0, dp(2));
            slotTabsMenu.addView(tabItem, itemParams);
        }
        body.addView(slotTabsMenu);

        if (selectedSlot != null) {
            final int si = selectedIndex;
            JSONObject sl = selectedSlot;
            String name = sl.optString("name");
            LinearLayout titleRow = new LinearLayout(this);
            titleRow.setOrientation(LinearLayout.HORIZONTAL);
            titleRow.setGravity(Gravity.CENTER_VERTICAL);
            titleRow.setLayoutParams(new LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT));

            TextView nameView = text(name, 15, TEXT, true);
            nameView.setSingleLine(true);
            nameView.setEllipsize(TextUtils.TruncateAt.END);
            // Leaves room for rename, the enable switch and delete at 360dp.
            nameView.setMaxWidth(Math.max(dp(60), getResources().getDisplayMetrics().widthPixels - dp(28 + 24 + 196)));
            titleRow.addView(nameView);

            ImageView renameIcon = new ImageView(this);
            renameIcon.setImageResource(R.drawable.clashvless_edit);
            renameIcon.setColorFilter(MUTED);
            int ripPad = dp(4);
            renameIcon.setPadding(ripPad, ripPad, ripPad, ripPad);
            LinearLayout.LayoutParams ripParams = new LinearLayout.LayoutParams(dp(24), dp(24));
            ripParams.setMargins(dp(8), 0, 0, 0);
            renameIcon.setLayoutParams(ripParams);
            renameIcon.setClickable(true);
            renameIcon.setFocusable(true);
            if (Build.VERSION.SDK_INT >= 26) {
                renameIcon.setTooltipText("Rename slot");
            }
            renameIcon.setContentDescription("Rename slot");
            renameIcon.setOnClickListener(v -> renameSlot(name));
            titleRow.addView(renameIcon);

            TextView sep = text("|", 13, MUTED, false);
            sep.setPadding(dp(8), 0, dp(8), 0);
            titleRow.addView(sep);

            boolean isSlotEnabled = sl.optBoolean("enabled");
            TextView statusText = text(isSlotEnabled ? "Enabled" : "Disabled", 13, isSlotEnabled ? TEXT : MUTED, false);
            statusText.setPadding(0, 0, dp(6), 0);
            titleRow.addView(statusText);

            Switch enableSwitch = new Switch(this);
            enableSwitch.setChecked(isSlotEnabled);
            if (Build.VERSION.SDK_INT >= 21) {
                enableSwitch.setThumbTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{ACCENT, MUTED}));
                enableSwitch.setTrackTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{Color.rgb(36, 103, 89), Color.rgb(51, 66, 88)}));
            }
            enableSwitch.setOnCheckedChangeListener((view, on) -> {
                statusText.setText(on ? "Enabled" : "Disabled");
                statusText.setTextColor(on ? TEXT : MUTED);
                selectedSlotName = name; // the VPN slot can change; stay on this one
                slotsMenuExpanded = true;
                editSlotNamed(name, (now, list, index, patch) -> { list.optJSONObject(index).put("enabled", on); return true; });
            });
            titleRow.addView(enableSwitch);

            View titleSpacer = new View(this);
            titleRow.addView(titleSpacer, new LinearLayout.LayoutParams(0, 1, 1.0f));

            ImageView deleteIcon = new ImageView(this);
            deleteIcon.setImageResource(R.drawable.clashvless_delete);
            deleteIcon.setColorFilter(MUTED);
            int delPad = dp(4);
            deleteIcon.setPadding(delPad, delPad, delPad, delPad);
            LinearLayout.LayoutParams delParams = new LinearLayout.LayoutParams(dp(24), dp(24));
            deleteIcon.setLayoutParams(delParams);
            deleteIcon.setClickable(true);
            deleteIcon.setFocusable(true);
            if (Build.VERSION.SDK_INT >= 26) {
                deleteIcon.setTooltipText("Delete slot");
            }
            deleteIcon.setContentDescription("Delete slot");
            deleteIcon.setOnClickListener(v -> removeSlot(name));
            titleRow.addView(deleteIcon);

            LinearLayout slot = card(titleRow, null);
            JSONObject live = liveSlot(s, name);
            slot.addView(slotStatus(sl.optBoolean("enabled"), live, sl.optInt("port"), hopPort(st, si), "proxy".equals(sl.optString("type"))), topMargin(10));
            Button vpnBtn = btn(name.equals(tunSlot) ? "Selected for VPN" : "Use for VPN", false,
                    name.equals(tunSlot) ? () -> {} : () -> Ctl.patch("tun_slot", name));
            if (name.equals(tunSlot)) {
                vpnBtn.setEnabled(false);
                vpnBtn.setTextColor(ACCENT);
            }
            slot.addView(equalRow(vpnBtn, btn("Edit port", false, () -> editPort(name))), topMargin(8));
            body.addView(slot, spaced());
            body.addView(exitsCard(sl, live), spaced());
        }

        body.addView(card("Routing compatibility", "Vision exits are direct-only. Plain VLESS and HTTP/SOCKS5 exits can use any hop; REALITY exits require a non-Vision hop."));
    }

    private static JSONObject liveSlot(JSONObject s, String name) {
        JSONArray slots = s == null ? null : s.optJSONArray("Slots");
        for (int i = 0; slots != null && i < slots.length(); i++) {
            if (name.equals(slots.optJSONObject(i).optString("Name"))) return slots.optJSONObject(i);
        }
        return new JSONObject();
    }

    /** The first-hop port: the first slot honors entry_port, every other slot uses port + 1. */
    private static int hopPort(JSONObject st, int index) {
        return hopPort(st.optJSONArray("slots"), st.optInt("entry_port"), index);
    }

    private static int hopPort(JSONArray slots, int entry, int index) {
        int port = slots.optJSONObject(index).optInt("port");
        if (index == 0 && entry > 0) return entry != port ? entry : 0;
        return port > 0 ? port + 1 : 0;
    }

    private static final String[] TIER_NAMES = {"", "Direct", "Country hop", "Bypass hop"};

    /** A slot's live state: {tag view, detail, detail color, tone}, e.g. "T1 Direct | 87 ms" and its route. */
    private Object[] liveState(boolean enabled, JSONObject live) {
        int tier = live.optInt("Tier");
        if (!enabled) return new Object[]{neutralChip("Off"), "this slot is not serving", MUTED, MUTED};
        if (live.optBoolean("HopOnly")) return new Object[]{tintChip("First hop only", WARNING), live.optString("Main") + " | no final exit reachable", TEXT, WARNING};
        if (tier > 0) {
            String label = "T" + tier + " " + TIER_NAMES[Math.min(tier, 3)];
            long ms = live.optLong("Egress") / 1_000_000;
            if (ms > 0) label += " | " + ms + " ms";
            String detail = live.optString("Entry").isEmpty() ? live.optString("Main") : live.optString("Entry") + " -> " + live.optString("Main");
            if (!live.optString("Note").isEmpty()) detail += " | " + live.optString("Note");
            return new Object[]{tintChip(label, ACCENT), detail, TEXT, ACCENT};
        }
        if (!live.optString("Err").isEmpty()) return new Object[]{tintChip("Down", DANGER), live.optString("Err"), TEXT, DANGER};
        return new Object[]{neutralChip("Checking"), "looking for a working exit...", MUTED, MUTED};
    }

    /** The slot's state in a box tinted by it: the state tag, the route or error, then its ports. */
    private View slotStatus(boolean enabled, JSONObject live, int port, int hop, boolean isProxy) {
        Object[] state = liveState(enabled, live);
        LinearLayout box = toned((int) state[3]);
        box.addView(flow((View) state[0]));
        TextView route = text((String) state[1], 13, (int) state[2], false);
        route.setMaxLines(2);
        route.setEllipsize(TextUtils.TruncateAt.END);
        box.addView(route, topMargin(4));
        FlowLayout ports = flow();
        if (isProxy) ports.addView(tintChip("HTTP/SOCKS", ACCENT));
        ports.addView(neutralChip("Port " + port));
        if (hop > 0) ports.addView(neutralChip("Hop " + hop));
        box.addView(ports, topMargin(4));
        return box;
    }

    /** Which tiers can use an exit, in a couple of words; usable is false when none can. */
    private static String mainUsage(JSONObject main, boolean[] usable) {
        String url = main.optString("url");
        boolean vision = url.contains("xtls-rprx-vision"), direct = main.optBoolean("allow_no_hop");
        usable[0] = main.optBoolean("enabled") && (direct || !vision);
        if (!main.optBoolean("enabled")) return "Disabled";
        if (vision) return direct ? "Direct only" : "Unused: Vision needs direct";
        return direct ? "Direct or hop" : "Hop only";
    }

    private View exitsCard(JSONObject sl, JSONObject live) {
        String slotName = sl.optString("name");
        JSONArray mains = sl.optJSONArray("mains");
        int count = mains == null ? 0 : mains.length();
        LinearLayout card = card((View) null, null);
        LinearLayout head = new LinearLayout(this);
        head.setGravity(Gravity.CENTER_VERTICAL);
        LinearLayout titles = new LinearLayout(this);
        titles.setOrientation(LinearLayout.VERTICAL);
        titles.addView(text("Final exits", 15, TEXT, true));
        titles.addView(text((count > 0 ? count + " in priority order: " : "In priority order: ") + "the top working exit is used", 11, MUTED, false));
        head.addView(titles, new LinearLayout.LayoutParams(0, -2, 1));
        Button export = btn("Export", false, () -> {
            try { Exports.call(this, new JSONObject().put("mains", mains), response -> showShare("Export | " + slotName, response)); } catch (Exception e) { failed(e); }
        });
        export.setEnabled(count > 0);
        if (count == 0) export.setAlpha(0.4f);
        head.addView(export);
        card.addView(head);
        if (count == 0) card.addView(text("No final exits in this slot yet.", 12, MUTED, false), topMargin(8));
        for (int m = 0; m < count; m++) card.addView(exitCard(slotName, mains.optJSONObject(m), m, count, live), topMargin(8));
        boolean isProxy = "proxy".equals(sl.optString("type"));
        card.addView(btn("Add final exits", true, () -> {
            if (isProxy) addProxyExits(slotName);
            else addExits(slotName);
        }), topMargin(10));
        return card;
    }

    private View exitCard(String slotName, JSONObject main, int index, int count, JSONObject live) {
        String name = main.optString("name"), url = main.optString("url");
        boolean enabled = main.optBoolean("enabled");
        boolean active = live.optInt("Tier") > 0 && !live.optBoolean("HopOnly") && name.equals(live.optString("Main"));
        LinearLayout box = new LinearLayout(this);
        box.setPadding(dp(4), dp(6), dp(6), dp(4));
        GradientDrawable background = round(BG, 8);
        background.setStroke(dp(1), active ? alpha(ACCENT, 150) : BORDER);
        box.setBackground(background);

        // Priority column: the number with its raise/lower arrows beneath.
        LinearLayout priority = new LinearLayout(this);
        priority.setOrientation(LinearLayout.VERTICAL);
        priority.setGravity(Gravity.CENTER_HORIZONTAL);
        TextView number = text(String.valueOf(index + 1), 12, active ? BG : TEXT, true);
        number.setGravity(Gravity.CENTER);
        number.setPadding(0, 0, 0, 0);
        GradientDrawable badge = new GradientDrawable();
        badge.setShape(GradientDrawable.OVAL);
        badge.setColor(active ? ACCENT : CONTROL);
        number.setBackground(badge);
        LinearLayout.LayoutParams badgeParams = new LinearLayout.LayoutParams(dp(24), dp(24));
        badgeParams.setMargins(0, dp(6), 0, dp(2));
        priority.addView(number, badgeParams);
        priority.addView(icon(R.drawable.clashvless_up, "Raise priority", index > 0, () -> editExit(slotName, url, (mains, i) -> swap(mains, i, i - 1))));
        priority.addView(icon(R.drawable.clashvless_down, "Lower priority", index < count - 1, () -> editExit(slotName, url, (mains, i) -> swap(mains, i, i + 1))));
        box.addView(priority, new LinearLayout.LayoutParams(dp(36), -2));

        LinearLayout info = new LinearLayout(this);
        info.setOrientation(LinearLayout.VERTICAL);
        info.setPadding(dp(6), 0, 0, 0);
        LinearLayout head = new LinearLayout(this);
        head.setGravity(Gravity.CENTER_VERTICAL);
        head.addView(oneLine(name, 14, enabled ? TEXT : MUTED, active), new LinearLayout.LayoutParams(0, -2, 1));
        head.addView(icon(R.drawable.clashvless_qr, "Share link / QR code", true, () -> {
            try { Exports.call(this, new JSONObject().put("mains", new JSONArray().put(main)), response -> showShare("Share exit", response)); } catch (Exception e) { failed(e); }
        }));
        head.addView(icon(R.drawable.clashvless_edit, "Rename exit", true, () -> ask("Rename exit", name, InputType.TYPE_CLASS_TEXT,
                value -> editExit(slotName, url, (mains, i) -> mains.getJSONObject(i).put("name", value.trim().replaceAll("\\s+", " "))))));
        head.addView(icon(R.drawable.clashvless_delete, "Delete exit", true, () -> confirm("Delete " + name + "?",
                () -> editExit(slotName, url, (mains, i) -> mains.remove(i)))));
        info.addView(head);

        boolean[] usable = {true};
        String usage = mainUsage(main, usable);
        FlowLayout tags = flow();
        if (active) {
            String label = "In use | T" + live.optInt("Tier");
            long ms = live.optLong("Egress") / 1_000_000;
            if (ms > 0) label += " | " + ms + " ms";
            tags.addView(tintChip(label, ACCENT));
        }
        tags.addView(neutralChip(proto(url)));
        tags.addView(!usable[0] && enabled ? tintChip(usage, DANGER) : neutralChip(usage));
        info.addView(tags);
        info.addView(oneLine(mainHost(url), 12, MUTED, false), topMargin(2));

        CheckBox on = check("Enabled", enabled);
        on.setOnCheckedChangeListener((view, value) -> editExit(slotName, url, (mains, i) -> mains.getJSONObject(i).put("enabled", value)));
        CheckBox direct = check("Allow direct (T1)", main.optBoolean("allow_no_hop"));
        direct.setOnCheckedChangeListener((view, value) -> editExit(slotName, url, (mains, i) -> mains.getJSONObject(i).put("allow_no_hop", value)));
        LinearLayout.LayoutParams togglesParams = new LinearLayout.LayoutParams(-1, -2);
        togglesParams.setMargins(-dp(6), dp(2), 0, 0); // the checkbox's own inset
        info.addView(flow(on, direct), togglesParams);
        box.addView(info, new LinearLayout.LayoutParams(0, -2, 1));
        return box;
    }

    /** The #fragment is only an exit's name: an exported link carries the shown name, so duplicates ignore it. */
    private static String withoutName(String url) {
        int hash = url.indexOf('#');
        return hash >= 0 ? url.substring(0, hash) : url;
    }

    private static void swap(JSONArray mains, int a, int b) throws Exception {
        if (b < 0 || b >= mains.length()) return;
        JSONObject first = mains.getJSONObject(a);
        mains.put(a, mains.getJSONObject(b));
        mains.put(b, first);
    }

    private interface ExitChange { void apply(JSONArray mains, int index) throws Exception; }

    /** Edits an exit found by slot name and URL in the latest state, so a reorder elsewhere can't hit the wrong one. */
    private void editExit(String slotName, String url, ExitChange change) {
        JSONObject st = Ctl.state;
        if (st == null) return;
        try {
            JSONArray slots = copy(st.optJSONArray("slots"));
            for (int s = 0; s < slots.length(); s++) {
                if (!slotName.equals(slots.optJSONObject(s).optString("name"))) continue;
                JSONArray mains = slots.optJSONObject(s).optJSONArray("mains");
                for (int m = 0; mains != null && m < mains.length(); m++) {
                    if (!url.equals(mains.optJSONObject(m).optString("url"))) continue;
                    change.apply(mains, m);
                    Ctl.patch("slots", slots);
                    return;
                }
            }
            toast("This exit was changed elsewhere");
        } catch (Exception e) { failed(e); }
    }

    /** Adds HTTP/SOCKS5 exits via either copy-paste or manual parameter input. */
    private void addProxyExits(String slotName) {
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), BORDER);
        root.setBackground(bg);
        TextView titleView = text("Add HTTP / SOCKS5 exits", 16, TEXT, true);
        titleView.setPadding(0, 0, 0, dp(10));
        root.addView(titleView);

        final boolean[] manualMode = {false};
        final String[] pasteProto = {"socks5"};
        final String[] manualProto = {"socks5"};

        Button pasteModeBtn = btn("Copy-paste", true, () -> {});
        Button manualModeBtn = btn("Manual", false, () -> {});
        LinearLayout modeRow = equalRow(pasteModeBtn, manualModeBtn);
        root.addView(modeRow);

        // Copy-paste view
        LinearLayout pasteBox = new LinearLayout(this);
        pasteBox.setOrientation(LinearLayout.VERTICAL);
        pasteBox.addView(text("Default protocol (when omitted)", 12, MUTED, false), topMargin(10));
        Button pasteSocksBtn = btn("SOCKS5", true, () -> {});
        Button pasteHttpBtn = btn("HTTP", false, () -> {});
        pasteBox.addView(equalRow(pasteSocksBtn, pasteHttpBtn), topMargin(4));
        EditText pasteInput = styledInput("socks5://user:pass@host:port\nhttp://host:port\nhost:port:user:pass",
                InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_MULTI_LINE | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        pasteInput.setGravity(Gravity.TOP | Gravity.START);
        pasteInput.setMinLines(4);
        pasteInput.setMaxLines(7);
        pasteBox.addView(pasteInput, topMargin(8));

        // Manual view
        LinearLayout manualBox = new LinearLayout(this);
        manualBox.setOrientation(LinearLayout.VERTICAL);
        manualBox.setVisibility(View.GONE);
        manualBox.addView(text("Protocol", 12, MUTED, false), topMargin(10));
        Button manSocksBtn = btn("SOCKS5", true, () -> {});
        Button manHttpBtn = btn("HTTP", false, () -> {});
        manualBox.addView(equalRow(manSocksBtn, manHttpBtn), topMargin(4));
        EditText hostInput = styledInput("Host (1.2.3.4 or proxy.example.com)", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        EditText portInput = styledInput("Port (1080)", InputType.TYPE_CLASS_NUMBER);
        EditText userInput = styledInput("Username (optional)", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        EditText passInput = styledInput("Password (optional)", InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_VARIATION_PASSWORD);
        EditText nameInput = styledInput("Name (optional, defaults to host:port)", InputType.TYPE_CLASS_TEXT);
        CheckBox directCheck = check("Allow direct (T1)", true);
        manualBox.addView(hostInput, topMargin(8));
        manualBox.addView(portInput, topMargin(6));
        manualBox.addView(userInput, topMargin(6));
        manualBox.addView(passInput, topMargin(6));
        manualBox.addView(nameInput, topMargin(6));
        manualBox.addView(directCheck, topMargin(6));

        Runnable refreshToggle = () -> {
            styleToggle(pasteModeBtn, !manualMode[0]);
            styleToggle(manualModeBtn, manualMode[0]);
            pasteBox.setVisibility(manualMode[0] ? View.GONE : View.VISIBLE);
            manualBox.setVisibility(manualMode[0] ? View.VISIBLE : View.GONE);
            styleToggle(pasteSocksBtn, "socks5".equals(pasteProto[0]));
            styleToggle(pasteHttpBtn, "http".equals(pasteProto[0]));
            styleToggle(manSocksBtn, "socks5".equals(manualProto[0]));
            styleToggle(manHttpBtn, "http".equals(manualProto[0]));
        };
        pasteModeBtn.setOnClickListener(v -> { manualMode[0] = false; refreshToggle.run(); });
        manualModeBtn.setOnClickListener(v -> { manualMode[0] = true; refreshToggle.run(); });
        pasteSocksBtn.setOnClickListener(v -> { pasteProto[0] = "socks5"; refreshToggle.run(); });
        pasteHttpBtn.setOnClickListener(v -> { pasteProto[0] = "http"; refreshToggle.run(); });
        manSocksBtn.setOnClickListener(v -> { manualProto[0] = "socks5"; refreshToggle.run(); });
        manHttpBtn.setOnClickListener(v -> { manualProto[0] = "http"; refreshToggle.run(); });
        refreshToggle.run();

        ScrollView scroll = new ScrollView(this);
        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        content.addView(pasteBox);
        content.addView(manualBox);
        scroll.addView(content);
        root.addView(scroll, new LinearLayout.LayoutParams(-1, -2));

        LinearLayout buttons = new LinearLayout(this);
        buttons.setGravity(Gravity.END);
        buttons.addView(btn("Cancel", false, dialog::dismiss));
        buttons.addView(new View(this), new LinearLayout.LayoutParams(dp(8), 1));
        buttons.addView(btn("Add", true, () -> {
            if (!Ctl.connected) { toast("Daemon not connected"); return; }
            if (!manualMode[0]) {
                String value = pasteInput.getText().toString().trim();
                if (value.isEmpty()) { toast("Enter at least one proxy"); return; }
                dialog.dismiss();
                try {
                    JSONObject req = new JSONObject().put("parse", value).put("slot_type", "proxy").put("default_scheme", pasteProto[0]);
                    Exports.call(this, req, response -> mergeParsedExits(slotName, response, "Enter at least one valid HTTP/SOCKS5 proxy", null));
                } catch (Exception e) { failed(e); }
                return;
            }
            String host = hostInput.getText().toString().trim();
            int port;
            try { port = Integer.parseInt(portInput.getText().toString().trim()); } catch (NumberFormatException e) { port = 0; }
            if (host.isEmpty()) { toast("Enter a proxy host"); return; }
            if (port < 1 || port > 65535) { toast("Port must be between 1 and 65535"); return; }
            boolean allowDirect = directCheck.isChecked();
            dialog.dismiss();
            try {
                JSONObject spec = new JSONObject()
                        .put("scheme", manualProto[0])
                        .put("host", host)
                        .put("port", port)
                        .put("user", userInput.getText().toString())
                        .put("pass", passInput.getText().toString())
                        .put("name", nameInput.getText().toString().trim());
                Exports.call(this, new JSONObject().put("build_proxy", spec),
                        response -> mergeParsedExits(slotName, response, "Invalid proxy parameters", allowDirect));
            } catch (Exception e) { failed(e); }
        }));
        root.addView(buttons, topMargin(14));

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            dialog.getWindow().setLayout(Math.min((int) (getResources().getDisplayMetrics().widthPixels * 0.92f), dp(420)), ViewGroup.LayoutParams.WRAP_CONTENT);
            dialog.getWindow().setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE);
        }
        dialog.show();
    }

    private void mergeParsedExits(String slotName, JSONObject response, String emptyError, Boolean allowDirectOverride) {
        String err = response.optString("error");
        if (!err.isEmpty()) { toast(err); return; }
        JSONArray exits = response.optJSONArray("exits");
        int invalid = response.optInt("invalid");
        if (exits == null || exits.length() == 0) { toast(emptyError); return; }
        JSONObject st = Ctl.state;
        JSONArray slots = copy(st == null ? null : st.optJSONArray("slots"));
        for (int s = 0; s < slots.length(); s++) {
            JSONObject slot = slots.optJSONObject(s);
            if (!slotName.equals(slot.optString("name"))) continue;
            try {
                JSONArray mains = slot.optJSONArray("mains");
                if (mains == null) slot.put("mains", mains = new JSONArray());
                Set<String> present = new HashSet<>();
                for (int m = 0; m < mains.length(); m++) present.add(withoutName(mains.optJSONObject(m).optString("url")));
                int added = 0, duplicate = 0;
                for (int e = 0; e < exits.length(); e++) {
                    JSONObject exit = exits.optJSONObject(e);
                    if (!present.add(withoutName(exit.optString("url")))) { duplicate++; continue; }
                    if (allowDirectOverride != null) exit.put("allow_no_hop", allowDirectOverride);
                    mains.put(exit);
                    added++;
                }
                if (added > 0) Ctl.patch("slots", slots);
                String message = "Added " + added + " exit(s) to " + slotName;
                if (duplicate > 0) message += " | " + duplicate + " already present";
                if (invalid > 0) message += " | " + invalid + " invalid line(s) skipped";
                toast(message);
            } catch (Exception e) { failed(e); }
            return;
        }
        toast("This slot was removed while editing");
    }

    private EditText styledInput(String hint, int inputType) {
        EditText input = new EditText(this);
        input.setInputType(inputType);
        input.setHint(hint);
        input.setTextColor(TEXT);
        input.setHintTextColor(MUTED);
        input.setTextSize(13);
        input.setPadding(dp(12), dp(10), dp(12), dp(10));
        GradientDrawable inputBg = round(CONTROL, 8);
        inputBg.setStroke(dp(1), BORDER);
        input.setBackground(inputBg);
        input.setOnFocusChangeListener((v, hasFocus) -> inputBg.setStroke(dp(1), hasFocus ? ACCENT : BORDER));
        return input;
    }

    private void styleToggle(Button button, boolean selected) {
        GradientDrawable content = round(selected ? ACCENT : Color.rgb(31, 39, 49), 8);
        button.setBackground(new RippleDrawable(ColorStateList.valueOf(selected ? 0x55ffffff : 0x3368d5bd), content, round(Color.WHITE, 8)));
        button.setTextColor(selected ? BG : TEXT);
    }

    /** Takes several links, one per line, or a base64 subscription; the Go side validates and names them. */
    private void addExits(String slotName) {
        askLines("Add final exits", "vless://... one per line, or a base64 subscription", value -> {
            try {
                Exports.call(this, new JSONObject().put("parse", value),
                        response -> mergeParsedExits(slotName, response, "Enter at least one valid vless:// link", null));
            } catch (Exception e) { failed(e); }
        });
    }

    /** Marzban-style export: a QR code per link with copy actions, and whole-list copies when there are several. */
    private void showShare(String title, JSONObject response) {
        JSONArray links = response.optJSONArray("links");
        int skipped = response.optInt("skipped");
        if (links == null || links.length() == 0) {
            toast(skipped > 0 ? "Only VLESS nodes can be exported" : "Nothing to export");
            return;
        }
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(18), dp(16), dp(18), dp(14));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), BORDER);
        root.setBackground(bg);
        root.addView(oneLine(title, 16, TEXT, true));

        LinearLayout content = new LinearLayout(this);
        content.setOrientation(LinearLayout.VERTICAL);
        LinearLayout nameRow = new LinearLayout(this);
        nameRow.setGravity(Gravity.CENTER_VERTICAL);
        TextView name = oneLine("", 14, TEXT, true);
        TextView counter = text("", 11, MUTED, false);
        nameRow.addView(name, new LinearLayout.LayoutParams(0, -2, 1));
        nameRow.addView(counter);
        content.addView(nameRow, topMargin(8));
        int dialogWidth = Math.min((int) (getResources().getDisplayMetrics().widthPixels * 0.92f), dp(400));
        int qrSide = Math.min(dialogWidth - dp(36), dp(280));
        ImageView qr = new ImageView(this);
        LinearLayout.LayoutParams qrParams = new LinearLayout.LayoutParams(qrSide, qrSide);
        qrParams.gravity = Gravity.CENTER_HORIZONTAL;
        qrParams.setMargins(0, dp(10), 0, dp(10));
        content.addView(qr, qrParams);
        TextView tooLong = text("This link is too long for a QR code; copy it instead.", 12, MUTED, false);
        content.addView(tooLong);
        TextView link = text("", 11, TEXT, false);
        link.setTextIsSelectable(true);
        link.setPadding(dp(8), dp(6), dp(8), dp(6));
        link.setBackground(round(BG, 6));
        content.addView(link);
        TextView note = text("", 11, WARNING, false);
        content.addView(note, topMargin(6));
        ScrollView scroller = new ScrollView(this);
        scroller.addView(content);
        root.addView(scroller, new LinearLayout.LayoutParams(-1, 0, 1));

        int[] index = {0};
        Button prev = btn("<", false, () -> { });
        Button next = btn(">", false, () -> { });
        Runnable show = () -> {
            JSONObject item = links.optJSONObject(index[0]);
            name.setText(item.optString("name"));
            counter.setText((index[0] + 1) + " of " + links.length());
            android.graphics.Bitmap bitmap = Exports.qr(item.optJSONObject("qr"), qrSide);
            qr.setImageBitmap(bitmap);
            qr.setVisibility(bitmap == null ? View.GONE : View.VISIBLE);
            tooLong.setVisibility(bitmap == null ? View.VISIBLE : View.GONE);
            link.setText(item.optString("url"));
            note.setText(item.optString("note"));
            note.setVisibility(item.optString("note").isEmpty() ? View.GONE : View.VISIBLE);
            prev.setEnabled(index[0] > 0);
            next.setEnabled(index[0] < links.length() - 1);
            prev.setAlpha(prev.isEnabled() ? 1 : 0.4f);
            next.setAlpha(next.isEnabled() ? 1 : 0.4f);
        };
        prev.setOnClickListener(v -> { index[0]--; show.run(); });
        next.setOnClickListener(v -> { index[0]++; show.run(); });
        Button copyLink = btn("Copy link", true, () -> copy("VLESS link", links.optJSONObject(index[0]).optString("url"), "Link copied"));
        LinearLayout actions = links.length() > 1 ? row(copyLink, prev, next) : row(copyLink);
        root.addView(actions, topMargin(10));
        if (links.length() > 1) {
            StringBuilder joined = new StringBuilder();
            for (int i = 0; i < links.length(); i++) joined.append(i > 0 ? "\n" : "").append(links.optJSONObject(i).optString("url"));
            String all = joined.toString();
            root.addView(text(links.length() + " links" + (skipped > 0 ? " | " + skipped + " non-VLESS nodes skipped" : ""), 11, MUTED, false), topMargin(6));
            root.addView(equalRow(
                    btn("Copy all links", false, () -> copy("VLESS links", all, links.length() + " links copied")),
                    btn("Copy as subscription", false, () -> copy("Subscription", android.util.Base64.encodeToString(
                            all.getBytes(java.nio.charset.StandardCharsets.UTF_8), android.util.Base64.NO_WRAP), "Base64 subscription copied"))));
        }
        LinearLayout close = new LinearLayout(this);
        close.setGravity(Gravity.END);
        close.addView(btn("Close", false, dialog::dismiss));
        root.addView(close, topMargin(8));
        show.run();

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            dialog.getWindow().setLayout(dialogWidth, ViewGroup.LayoutParams.WRAP_CONTENT); // the scroller shrinks when it can't fit
        }
        dialog.show();
    }

    private void askLines(String title, String hint, Consumer<String> ok) {
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), BORDER);
        root.setBackground(bg);
        TextView titleView = text(title, 16, TEXT, true);
        titleView.setPadding(0, 0, 0, dp(12));
        root.addView(titleView);
        EditText input = new EditText(this);
        input.setInputType(InputType.TYPE_CLASS_TEXT | InputType.TYPE_TEXT_FLAG_MULTI_LINE | InputType.TYPE_TEXT_FLAG_NO_SUGGESTIONS);
        input.setHint(hint);
        input.setGravity(Gravity.TOP | Gravity.START);
        input.setMinLines(4);
        input.setMaxLines(8);
        input.setTextColor(TEXT);
        input.setHintTextColor(MUTED);
        input.setTextSize(13);
        input.setPadding(dp(12), dp(10), dp(12), dp(10));
        GradientDrawable inputBg = round(CONTROL, 8);
        inputBg.setStroke(dp(1), BORDER);
        input.setBackground(inputBg);
        input.setOnFocusChangeListener((v, hasFocus) -> inputBg.setStroke(dp(1), hasFocus ? ACCENT : BORDER));
        root.addView(input, new LinearLayout.LayoutParams(-1, -2));
        LinearLayout buttons = new LinearLayout(this);
        buttons.setGravity(Gravity.END);
        buttons.addView(btn("Cancel", false, dialog::dismiss));
        buttons.addView(new View(this), new LinearLayout.LayoutParams(dp(8), 1));
        buttons.addView(btn("Add", true, () -> {
            String value = input.getText().toString().trim();
            if (value.isEmpty()) return;
            if (!Ctl.connected) { toast("Daemon not connected"); return; }
            dialog.dismiss();
            ok.accept(value);
        }));
        root.addView(buttons, topMargin(16));
        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            dialog.getWindow().setLayout(Math.min((int) (getResources().getDisplayMetrics().widthPixels * 0.92f), dp(420)), ViewGroup.LayoutParams.WRAP_CONTENT);
            dialog.getWindow().setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_STATE_ALWAYS_VISIBLE);
        }
        dialog.show();
        input.requestFocus();
    }

    private TextView chip(String value, int fg, int bg) {
        TextView chip = new TextView(this);
        chip.setText(value);
        chip.setTextSize(11);
        chip.setTextColor(fg);
        chip.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        chip.setSingleLine(true);
        chip.setEllipsize(TextUtils.TruncateAt.END);
        chip.setPadding(dp(6), dp(1), dp(6), dp(2));
        chip.setBackground(round(bg, 4));
        return chip;
    }

    private TextView neutralChip(String value) {
        return chip(value, MUTED, CONTROL);
    }

    private TextView tintChip(String value, int color) {
        return chip(value, color, alpha(color, 40));
    }

    private static int alpha(int color, int alpha) {
        return Color.argb(alpha, Color.red(color), Color.green(color), Color.blue(color));
    }

    /** Sets a block apart with a soft tint and border instead of separator lines. */
    private LinearLayout toned(int tone) {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(8), dp(10), dp(8));
        GradientDrawable shape = round(alpha(tone, 22), 8);
        shape.setStroke(dp(1), alpha(tone, 80));
        box.setBackground(shape);
        return box;
    }

    private LinearLayout innerBox() {
        LinearLayout box = new LinearLayout(this);
        box.setOrientation(LinearLayout.VERTICAL);
        box.setPadding(dp(10), dp(8), dp(6), dp(8));
        box.setBackground(round(BG, 8));
        box.setLayoutParams(topMargin(8));
        return box;
    }

    private FlowLayout flow(View... views) {
        FlowLayout flow = new FlowLayout(this, dp(6));
        for (View view : views) flow.addView(view);
        return flow;
    }

    private TextView oneLine(String value, int size, int color, boolean bold) {
        TextView view = text(value, size, color, bold);
        view.setSingleLine(true);
        view.setEllipsize(TextUtils.TruncateAt.END);
        return view;
    }

    private CheckBox check(String label, boolean checked) {
        CheckBox box = new CheckBox(this);
        box.setText(label);
        box.setTextColor(TEXT);
        box.setTextSize(13);
        box.setChecked(checked);
        box.setButtonTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{ACCENT, MUTED}));
        return box;
    }

    private ImageView icon(int resource, String hint, boolean enabled, Runnable action) {
        ImageView icon = new ImageView(this);
        icon.setImageResource(resource);
        icon.setColorFilter(MUTED);
        icon.setAlpha(enabled ? 1f : 0.3f);
        icon.setPadding(dp(7), dp(7), dp(7), dp(7));
        icon.setLayoutParams(new LinearLayout.LayoutParams(dp(36), dp(36)));
        icon.setContentDescription(hint);
        if (Build.VERSION.SDK_INT >= 26) icon.setTooltipText(hint);
        icon.setEnabled(enabled);
        if (enabled) {
            icon.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), null, round(Color.WHITE, 18)));
            icon.setOnClickListener(v -> action.run());
        }
        return icon;
    }

    private LinearLayout.LayoutParams topMargin(int top) {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(-1, -2);
        params.setMargins(0, dp(top), 0, 0);
        return params;
    }

    private LinearLayout.LayoutParams spaced() {
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(-1, -2);
        params.setMargins(0, 0, 0, dp(12));
        return params;
    }


    private void renderLog() {
        LinearLayout filters = card("Log filters", "Choose severity and module, or search with & and |");
        filters.addView(row(btn("Levels", false, this::chooseLogLevels),
                btn(logModule.isEmpty() ? "All modules" : logModule, false, this::chooseLogModule)));
        EditText query = new EditText(this);
        query.setSingleLine(true);
        query.setHint("Search: route & failed | timeout");
        query.setHintTextColor(MUTED);
        query.setTextColor(TEXT);
        query.setTextSize(13);
        query.setPadding(dp(10), 0, dp(10), 0);
        query.setBackground(round(Color.rgb(32, 40, 51), 8));
        query.setText(logQuery);
        query.addTextChangedListener(new TextWatcher() {
            public void beforeTextChanged(CharSequence s, int start, int count, int after) { }
            public void onTextChanged(CharSequence s, int start, int before, int count) { }
            public void afterTextChanged(Editable s) { logQuery = s.toString(); refreshLog(); }
        });
        filters.addView(query, new LinearLayout.LayoutParams(-1, dp(46)));
        body.addView(filters);

        LinearLayout logs = card("Activity", "Live engine and xray events | newest at the bottom");
        logs.addView(btn("Clear view", false, () -> { Core.clearLog(); refreshLog(); }));
        logView = text("", 12, MUTED, false);
        logView.setLineSpacing(dp(3), 1f);
        logView.setTextIsSelectable(true);
        logs.addView(logView);
        body.addView(logs);
        Core.setListener(line -> {
            if (logQueued) return;
            logQueued = true;
            body.postDelayed(() -> { logQueued = false; refreshLog(); }, 250);
        });
        refreshLog();
        scroll.post(() -> scroll.scrollTo(0, body.getHeight()));
    }

    /** Keeps following new lines only while the view is already at the bottom. */
    private void refreshLog() {
        if (tab != LOG || logView == null) return;
        boolean atEnd = scroll.getScrollY() + scroll.getHeight() >= body.getHeight() - dp(48);
        logView.setText(styledLog(Core.filteredLog(logLevels, logModule, logQuery)));
        if (atEnd) scroll.post(() -> scroll.scrollTo(0, body.getHeight()));
    }

    /** Muted time, a colored level and the origin, then the message, like the desktop's columns. */
    private CharSequence styledLog(List<Core.Entry> entries) {
        android.text.SpannableStringBuilder out = new android.text.SpannableStringBuilder();
        for (Core.Entry entry : entries) {
            if (out.length() > 0) out.append('\n');
            if (!entry.structured) {
                span(out, entry.line, entry.line.startsWith("[") ? TEXT : MUTED, false);
                continue;
            }
            String[] parts = entry.line.split(" +", 4);
            int levelColor = "ERROR".equals(entry.level) ? DANGER : "WARN".equals(entry.level) ? WARNING : "DEBUG".equals(entry.level) ? MUTED : BLUE;
            span(out, parts[0].substring(0, 8) + " ", MUTED, false);
            span(out, entry.level + " ", levelColor, true);
            span(out, parts[2] + "  ", MUTED, false);
            span(out, parts[3], "ERROR".equals(entry.level) ? DANGER : "WARN".equals(entry.level) ? WARNING : TEXT, false);
        }
        return out;
    }

    private static void span(android.text.SpannableStringBuilder out, String text, int color, boolean bold) {
        int start = out.length();
        out.append(text);
        out.setSpan(new android.text.style.ForegroundColorSpan(color), start, out.length(), android.text.Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
        if (bold) out.setSpan(new android.text.style.StyleSpan(Typeface.BOLD), start, out.length(), android.text.Spanned.SPAN_EXCLUSIVE_EXCLUSIVE);
    }

    private void chooseLogLevels() {
        String[] names = {"Info", "Warning", "Error", "Debug"};
        String[] values = {"INFO", "WARN", "ERROR", "DEBUG"};
        boolean[] checked = new boolean[values.length];
        for (int i = 0; i < values.length; i++) checked[i] = logLevels.contains(values[i]);
        new AlertDialog.Builder(this, android.R.style.Theme_Material_Dialog_Alert).setTitle("Visible log levels")
                .setMultiChoiceItems(names, checked, (dialog, which, on) -> checked[which] = on)
                .setPositiveButton("Apply", (dialog, which) -> {
                    logLevels.clear();
                    for (int i = 0; i < values.length; i++) if (checked[i]) logLevels.add(values[i]);
                    refreshLog();
                }).setNegativeButton("Cancel", null).show();
    }

    private void chooseLogModule() {
        List<String> modules = new ArrayList<>();
        modules.add("All modules");
        modules.addAll(Core.modules());
        String[] items = modules.toArray(new String[0]);
        choose("Log module", items, which -> {
            logModule = which == 0 ? "" : items[which];
            render();
        });
    }

    private void renderConfig(JSONObject st) {
        String[] names = {"Network", "VPN", "Exit routes", "Providers & nodes", "Updates", "Device"};
        String[] descriptions = {
                "Listeners, health checks, and failover",
                "System-wide tunnel and interface",
                "Failover slots and final VLESS exits",
                "Subscriptions and entry-node selection",
                "Background refresh and fetch proxy",
                "Hardware ID and panel identity"
        };
        LinearLayout sectionGroup = new LinearLayout(this);
        sectionGroup.setOrientation(LinearLayout.VERTICAL);
        sectionGroup.setPadding(dp(14), dp(10), dp(14), dp(12));
        GradientDrawable groupBackground = round(Color.TRANSPARENT, 10);
        groupBackground.setStroke(dp(1), Color.rgb(43, 53, 65));
        sectionGroup.setBackground(groupBackground);
        sectionGroup.addView(text("SETTINGS TYPE", 11, ACCENT, true));
        sectionGroup.addView(text("Choose a category", 11, MUTED, false));
        Button currentSection = btn(names[configSection], false, () -> { });
        currentSection.setTextSize(14);
        currentSection.setGravity(Gravity.START | Gravity.CENTER_VERTICAL);
        currentSection.setPadding(dp(11), 0, dp(48), 0);
        FrameLayout sectionSelector = new FrameLayout(this);
        sectionSelector.addView(currentSection, new FrameLayout.LayoutParams(-1, -1));
        ImageView sectionIndicator = new ImageView(this);
        sectionIndicator.setImageResource(R.drawable.clashvless_expand);
        sectionIndicator.setElevation(dp(4));
        sectionIndicator.setContentDescription("Expand settings types");
        FrameLayout.LayoutParams indicatorParams = new FrameLayout.LayoutParams(dp(24), dp(24));
        indicatorParams.gravity = Gravity.END | Gravity.CENTER_VERTICAL;
        indicatorParams.setMargins(0, 0, dp(14), 0);
        sectionSelector.addView(sectionIndicator, indicatorParams);
        LinearLayout.LayoutParams currentParams = new LinearLayout.LayoutParams(-1, dp(48));
        currentParams.setMargins(0, dp(8), 0, 0);
        sectionGroup.addView(sectionSelector, currentParams);
        LinearLayout sectionList = new LinearLayout(this);
        sectionList.setOrientation(LinearLayout.VERTICAL);
        sectionList.setVisibility(View.GONE);
        for (int i = 0; i < names.length; i++) {
            final int selected = i;
            boolean active = i == configSection;
            LinearLayout option = new LinearLayout(this);
            option.setOrientation(LinearLayout.HORIZONTAL);
            option.setGravity(Gravity.CENTER_VERTICAL);
            option.setPadding(dp(12), dp(6), dp(12), dp(6));
            option.setMinimumHeight(dp(54));
            GradientDrawable optionBackground = round(active ? Color.rgb(27, 37, 44) : Color.TRANSPARENT, 8);
            option.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), optionBackground, round(Color.WHITE, 8)));
            LinearLayout labels = new LinearLayout(this);
            labels.setOrientation(LinearLayout.VERTICAL);
            labels.addView(text(names[i], 14, active ? ACCENT : TEXT, true));
            labels.addView(text(descriptions[i], 11, MUTED, false));
            option.addView(labels, new LinearLayout.LayoutParams(0, -2, 1));
            if (active) option.addView(text("CURRENT", 10, ACCENT, true));
            option.setOnClickListener(view -> {
                configSection = selected;
                render();
            });
            LinearLayout.LayoutParams optionParams = new LinearLayout.LayoutParams(-1, -2);
            optionParams.setMargins(0, dp(2), 0, dp(2));
            sectionList.addView(option, optionParams);
        }
        LinearLayout.LayoutParams listParams = new LinearLayout.LayoutParams(-1, -2);
        listParams.setMargins(0, dp(4), 0, 0);
        sectionGroup.addView(sectionList, listParams);
        currentSection.setOnClickListener(view -> toggleConfigSections(sectionList, sectionIndicator));
        LinearLayout.LayoutParams groupParams = new LinearLayout.LayoutParams(-1, -2);
        groupParams.setMargins(0, 0, 0, dp(10));
        body.addView(sectionGroup, groupParams);

        if (configSection == 0) {
            LinearLayout failover = card("Automatic failover", "Route selection, health checks, and recovery");
            intRow(failover, st, "Probe interval | seconds", "interval_s", 12, 2, 600);
            intRow(failover, st, "Egress timeout | seconds", "timeout_s", 6, 1, 60);
            intRow(failover, st, "Recovery threshold", "up_threshold", 3, 1, 10);
            intRow(failover, st, "Failure threshold", "down_threshold", 2, 1, 10);
            String[] tiers = {"Automatic", "T1 | Direct", "T2 | Country", "T3 | Bypass"};
            chooseRow(failover, "Pin tier", tiers[st.optInt("pin_tier")], "Pin tier", tiers, which -> Ctl.patch("pin_tier", which));
            boolRow(failover, st, "Force a hop | skip T1", "force_hop", false);
            body.addView(failover);

            LinearLayout network = card("Local listeners", "Slot ports are set per slot in Exit routes");
            intRow(network, st, "First-hop port of the first slot | 0 = port + 1", "entry_port", 0, 0, 65535, port -> {
                if (port > 0 && port < 1024) return "Port must be 0 or between 1024 and 65535";
                JSONArray slots = copy(st.optJSONArray("slots"));
                if (port > 0 && slots.length() > 0 && slots.optJSONObject(0).optInt("port") == port) return "Port " + port + " is the first slot's own port";
                return portClash(slots, port);
            });
            boolean lan = !"127.0.0.1".equals(st.optString("listen_addr"));
            network.addView(toggle("Allow LAN connections", lan, on -> {
                Ctl.patch("listen_addr", on ? "0.0.0.0" : "127.0.0.1");
                restartEngine();
            }));
            network.addView(text(lan ? "Other devices on this network can use the SOCKS5 ports" : "Only apps on this phone can use the SOCKS5 ports", 12, MUTED, false));
            String[] levels = {"none", "error", "warning", "info", "debug"};
            String level = st.optString("log_level").isEmpty() ? "warning" : st.optString("log_level");
            chooseRow(network, "Xray log level", level, "Xray log level", levels, which -> Ctl.patch("log_level", levels[which]));
            body.addView(network);
        } else if (configSection == 1) {
            LinearLayout tun = card("System-wide VPN", "Static routed DNS | applied when the VPN reconnects");
            textRow(tun, st, "Static DNS resolver", "tun_static_dns", "8.8.8.8", value -> ipAddress(value) ? null : "Enter an IP address, like 8.8.8.8");
            boolRow(tun, st, "Answer ping locally", "tun_no_icmp", true);
            textRow(tun, st, "TUN interface address", "tun_addr", "198.18.0.1/30",
                    value -> value.matches(IPV4 + "/([8-9]|[12]\\d|30)") ? null : "Enter an IPv4 network, like 198.18.0.1/30");
            intRow(tun, st, "TUN MTU | 0 = default", "tun_mtu", 0, 0, 9000, mtu -> mtu == 0 || mtu >= 576 ? null : "MTU must be 0 or between 576 and 9000");
            body.addView(tun);
        } else if (configSection == 2) {
            renderMain(st);
        } else if (configSection == 3) {
            renderSubs(st);
        } else if (configSection == 4) {
            LinearLayout updates = card("Subscription updates", "Background refresh and optional fetch proxy");
            boolRow(updates, st, "Automatic refresh", "no_auto_fetch", true);
            intRow(updates, st, "Refresh interval | hours", "fetch_interval_h", 24, 1, 168);
            boolRow(updates, st, "Use fetch proxy", "use_fetch_proxy", false);
            textRow(updates, st, "Fetch proxy", "fetch_proxy", "socks5://127.0.0.1:2084");
            body.addView(updates);
        } else {
            LinearLayout identity = card("Device identity", "Stable Happ-compatible identity for every panel");
            JSONObject device = st.optJSONObject("device");
            String hwid = device == null ? "" : device.optString("hwid");
            actionRow(identity, "Hardware ID", hwid, () -> ask("Hardware ID", hwid, InputType.TYPE_CLASS_TEXT, value -> {
                if (!value.matches("[a-zA-Z0-9=-]{10,64}")) { toast("HWID must be 10-64 letters, digits, = or -"); return; }
                try { Ctl.patch("device", new JSONObject(device.toString()).put("hwid", value)); } catch (Exception e) { failed(e); }
            }));
            if (device != null) {
                identity.addView(text("Panel OS: " + device.optString("os") + " " + device.optString("os_ver")
                        + "\nModel: " + device.optString("model") + "\nUser agent: " + device.optString("ua")
                        + "\nLocale: " + device.optString("locale"), 11, MUTED, false));
            }
            body.addView(identity);
        }
    }

    private void toggleConfigSections(LinearLayout list, ImageView indicator) {
        Object running = list.getTag();
        if (running instanceof android.animation.ValueAnimator) ((android.animation.ValueAnimator) running).cancel();
        boolean opening = list.getVisibility() != View.VISIBLE;
        sectionsOpen = opening;
        indicator.animate().cancel();
        indicator.animate().rotation(opening ? 180 : 0).setDuration(220).start();
        indicator.setContentDescription(opening ? "Collapse settings types" : "Expand settings types");
        int start = list.getHeight();
        int target = 0;
        if (opening) {
            View parent = (View) list.getParent();
            int width = parent.getWidth() - parent.getPaddingLeft() - parent.getPaddingRight();
            list.measure(View.MeasureSpec.makeMeasureSpec(width, View.MeasureSpec.EXACTLY), View.MeasureSpec.UNSPECIFIED);
            target = list.getMeasuredHeight();
            start = 0;
            list.setVisibility(View.VISIBLE);
        }
        android.view.ViewGroup.LayoutParams initial = list.getLayoutParams();
        initial.height = start;
        list.setLayoutParams(initial);
        android.animation.ValueAnimator animator = android.animation.ValueAnimator.ofInt(start, target);
        animator.setDuration(220);
        animator.setInterpolator(new android.view.animation.DecelerateInterpolator());
        int startHeight = start;
        int range = target - startHeight;
        animator.addUpdateListener(animation -> {
            int height = (int) animation.getAnimatedValue();
            android.view.ViewGroup.LayoutParams params = list.getLayoutParams();
            params.height = height;
            list.setLayoutParams(params);
            float progress = range == 0 ? 1 : (float) (height - startHeight) / range;
            list.setAlpha(opening ? progress : 1 - progress);
        });
        animator.addListener(new android.animation.AnimatorListenerAdapter() {
            @Override
            public void onAnimationEnd(android.animation.Animator animation) {
                if (!opening) list.setVisibility(View.GONE);
                android.view.ViewGroup.LayoutParams params = list.getLayoutParams();
                params.height = -2;
                list.setLayoutParams(params);
                list.setAlpha(1);
                list.setTag(null);
            }
        });
        list.setTag(animator);
        animator.start();
    }

    private void toggleVpn() {
        if (VpnSvc.running) {
            startService(new Intent(this, VpnSvc.class).setAction(VpnSvc.ACTION_STOP));
            return;
        }
        Intent prep = VpnService.prepare(this);
        if (prep != null) startActivityForResult(prep, 1);
        else onActivityResult(1, RESULT_OK, null);
    }

    @Override
    protected void onActivityResult(int request, int result, Intent data) {
        if (request != 1) return;
        if (result != RESULT_OK) { renderHead(); return; }
        Intent intent = new Intent(this, VpnSvc.class).setAction(VpnSvc.ACTION_START);
        if (Build.VERSION.SDK_INT >= 26) startForegroundService(intent);
        else startService(intent);
    }

    private void addSub() {
        ask("Subscription URL", "", InputType.TYPE_TEXT_VARIATION_URI, value -> {
            if (!value.startsWith("http://") && !value.startsWith("https://")) { toast("Enter a valid http(s) URL"); return; }
            if (Ctl.cmd("addsub", "url", value)) fetching("adding subscription");
        });
    }

    private void addSlot() {
        JSONObject st = Ctl.state;
        if (st == null) return;
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);
        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), BORDER);
        root.setBackground(bg);
        TextView titleView = text("Add failover slot", 16, TEXT, true);
        titleView.setPadding(0, 0, 0, dp(10));
        root.addView(titleView);

        EditText nameInput = styledInput("Slot name", InputType.TYPE_CLASS_TEXT);
        nameInput.setText("slot" + (copy(st.optJSONArray("slots")).length() + 1));
        root.addView(nameInput);

        root.addView(text("Slot type", 12, MUTED, false), topMargin(12));
        final boolean[] isProxy = {false};
        Button vlessBtn = btn("VLESS", true, () -> {});
        Button proxyBtn = btn("HTTP / SOCKS5", false, () -> {});
        Runnable refresh = () -> {
            styleToggle(vlessBtn, !isProxy[0]);
            styleToggle(proxyBtn, isProxy[0]);
        };
        vlessBtn.setOnClickListener(v -> { isProxy[0] = false; refresh.run(); });
        proxyBtn.setOnClickListener(v -> { isProxy[0] = true; refresh.run(); });
        refresh.run();
        root.addView(equalRow(vlessBtn, proxyBtn), topMargin(6));

        LinearLayout buttons = new LinearLayout(this);
        buttons.setGravity(Gravity.END);
        buttons.addView(btn("Cancel", false, dialog::dismiss));
        buttons.addView(new View(this), new LinearLayout.LayoutParams(dp(8), 1));
        buttons.addView(btn("Add", true, () -> {
            String name = nameInput.getText().toString().trim();
            if (!validSlotName(name)) return;
            if (!Ctl.connected) { toast("Daemon not connected"); return; }
            JSONObject now = Ctl.state;
            try {
                JSONArray slots = copy(now.optJSONArray("slots"));
                for (int i = 0; i < slots.length(); i++) {
                    if (name.equals(slots.optJSONObject(i).optString("name"))) { toast("Slot name already exists"); return; }
                }
                JSONObject slot = new JSONObject().put("name", name).put("port", freePort(now)).put("enabled", true).put("mains", new JSONArray());
                if (isProxy[0]) slot.put("type", "proxy");
                slots.put(slot);
                slotsMenuExpanded = true;
                selectedSlotName = name;
                dialog.dismiss();
                Ctl.patch("slots", slots);
            } catch (Exception e) { failed(e); }
        }));
        root.addView(buttons, topMargin(16));

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            dialog.getWindow().setLayout(Math.min((int) (getResources().getDisplayMetrics().widthPixels * 0.92f), dp(420)), ViewGroup.LayoutParams.WRAP_CONTENT);
            dialog.getWindow().setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_STATE_ALWAYS_VISIBLE);
        }
        dialog.show();
        nameInput.requestFocus();
    }

    private boolean validSlotName(String name) {
        if (name.isEmpty()) { toast("Enter a valid slot name"); return false; }
        if (name.contains("://")) { toast("Slot name cannot contain ://"); return false; }
        return true;
    }

    private interface SlotChange { boolean apply(JSONObject st, JSONArray slots, int index, JSONObject patch) throws Exception; }

    /** Edits a slot by name on the latest state, so an edit made elsewhere meanwhile isn't reverted. */
    private void editSlotNamed(String name, SlotChange change) {
        JSONObject st = Ctl.state;
        if (st == null) return;
        try {
            JSONArray slots = copy(st.optJSONArray("slots"));
            for (int i = 0; i < slots.length(); i++) {
                if (!name.equals(slots.optJSONObject(i).optString("name"))) continue;
                JSONObject patch = new JSONObject();
                if (change.apply(st, slots, i, patch)) Ctl.patch(patch.put("slots", slots));
                return;
            }
            toast("This slot was changed elsewhere");
        } catch (Exception e) { failed(e); }
    }

    private void renameSlot(String oldName) {
        ask("Rename slot", oldName, InputType.TYPE_CLASS_TEXT, value -> {
            String name = value.trim();
            if (!validSlotName(name) || name.equals(oldName)) return;
            editSlotNamed(oldName, (st, slots, si, patch) -> {
                for (int i = 0; i < slots.length(); i++) {
                    if (i != si && name.equals(slots.optJSONObject(i).optString("name"))) { toast("Slot name already exists"); return false; }
                }
                slots.optJSONObject(si).put("name", name);
                if (oldName.equals(st.optString("tun_slot"))) patch.put("tun_slot", name);
                if (oldName.equals(selectedSlotName)) selectedSlotName = name;
                return true;
            });
        });
    }

    private void editPort(String name) {
        JSONObject current = findSlot(name);
        if (current == null) return;
        ask("Slot SOCKS5 port", "" + current.optInt("port"), InputType.TYPE_CLASS_NUMBER, value -> editSlotNamed(name, (st, slots, si, patch) -> {
            int port;
            try { port = Integer.parseInt(value); } catch (NumberFormatException e) { port = 0; }
            if (port < 1024 || port > 65534) { toast("Port must be between 1024 and 65534"); return false; }
            slots.optJSONObject(si).put("port", port);
            String clash = portClash(slots, st.optInt("entry_port"));
            if (clash != null) { toast(clash); return false; }
            return true;
        }));
    }

    /** Each slot claims its port and its first-hop port; no two claims may meet. */
    private static String portClash(JSONArray slots, int entryPort) {
        java.util.Map<Integer, String> claimed = new java.util.HashMap<>();
        for (int i = 0; i < slots.length(); i++) {
            String name = slots.optJSONObject(i).optString("name");
            int port = slots.optJSONObject(i).optInt("port"), hop = hopPort(slots, entryPort, i);
            for (int p : new int[]{port, hop}) {
                if (p <= 0) continue;
                String owner = claimed.put(p, name);
                if (owner != null) return "Port " + p + " is already used by " + (owner.equals(name) ? "this slot's first hop" : owner);
            }
        }
        return null;
    }

    private static JSONObject findSlot(String name) {
        JSONArray slots = Ctl.state == null ? null : Ctl.state.optJSONArray("slots");
        for (int i = 0; slots != null && i < slots.length(); i++) if (name.equals(slots.optJSONObject(i).optString("name"))) return slots.optJSONObject(i);
        return null;
    }

    private void removeSlot(String name) {
        confirm("Delete " + name + "?", () -> editSlotNamed(name, (st, slots, si, patch) -> {
            if (slots.length() <= 1) { toast("The last slot cannot be removed"); return false; }
            slots.remove(si);
            if (name.equals(selectedSlotName)) selectedSlotName = tunSlot(st);
            return true;
        }));
    }

    private void intRow(LinearLayout parent, JSONObject st, String label, String key, int fallback, int min, int max) {
        intRow(parent, st, label, key, fallback, min, max, null);
    }

    /** check returns why a number in range is still refused, or null. */
    private void intRow(LinearLayout parent, JSONObject st, String label, String key, int fallback, int min, int max,
                        java.util.function.IntFunction<String> check) {
        int current = st.optInt(key);
        if (current == 0 && fallback != 0) current = fallback;
        final int shown = current;
        actionRow(parent, label, "" + shown, () -> ask(label, "" + shown, InputType.TYPE_CLASS_NUMBER, value -> {
            try {
                int number = Integer.parseInt(value);
                if (number < min || number > max) { toast("Value must be between " + min + " and " + max); return; }
                String refused = check == null ? null : check.apply(number);
                if (refused != null) { toast(refused); return; }
                Ctl.patch(key, number);
            } catch (NumberFormatException e) { toast("Enter a valid number"); }
        }));
    }

    private void textRow(LinearLayout parent, JSONObject st, String label, String key, String hint) {
        textRow(parent, st, label, key, hint, null);
    }

    /** check returns why a value is refused, or null; an empty value always means the default. */
    private void textRow(LinearLayout parent, JSONObject st, String label, String key, String hint, java.util.function.Function<String, String> check) {
        String current = st.optString(key);
        actionRow(parent, label, current.isEmpty() ? hint : current, () -> ask(label, current, InputType.TYPE_TEXT_VARIATION_URI, value -> {
            String refused = check == null || value.isEmpty() ? null : check.apply(value);
            if (refused != null) { toast(refused); return; }
            Ctl.patch(key, value);
        }));
    }

    private static final String IPV4 = "((25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)\\.){3}(25[0-5]|2[0-4]\\d|1\\d\\d|[1-9]?\\d)";

    private static boolean ipAddress(String value) {
        return value.matches(IPV4) || (value.contains(":") && value.matches("[0-9a-fA-F:.]+"));
    }

    private void boolRow(LinearLayout parent, JSONObject st, String label, String key, boolean inverted) {
        boolean on = st.optBoolean(key) != inverted;
        parent.addView(toggle(label, on, value -> Ctl.patch(key, value == inverted)));
    }

    private void chooseRow(LinearLayout parent, String label, String value, String title, String[] items, IntConsumer pick) {
        actionRow(parent, label, value, () -> choose(title, items, pick));
    }

    private void actionRow(LinearLayout parent, String label, String value, Runnable action) {
        LinearLayout texts = new LinearLayout(this);
        texts.setOrientation(LinearLayout.VERTICAL);
        texts.addView(text(label, 13, TEXT, false));
        texts.addView(text(value, 12, MUTED, false));
        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        row.setPadding(0, dp(4), 0, dp(4));
        row.addView(texts, new LinearLayout.LayoutParams(0, -2, 1));
        LinearLayout.LayoutParams buttonParams = new LinearLayout.LayoutParams(-2, -2);
        buttonParams.setMargins(dp(8), 0, 0, 0);
        row.addView(btn("Edit", false, action), buttonParams);
        parent.addView(row);
    }

    /** The listen address is read when the engine starts; the VPN restarts it on Connect. */
    private void restartEngine() {
        if (VpnSvc.running) { toast("Applies the next time you connect"); return; }
        body.postDelayed(() -> new Thread(Core::stopIdle).start(), 800);
    }

    private Switch toggle(String label, boolean checked, Consumer<Boolean> changed) {
        Switch sw = new Switch(this);
        sw.setText(label);
        sw.setTextColor(TEXT);
        sw.setTextSize(13);
        sw.setPadding(0, dp(4), 0, dp(4));
        sw.setMinHeight(dp(44));
        sw.setChecked(checked);
        if (Build.VERSION.SDK_INT >= 21) {
            sw.setThumbTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{ACCENT, MUTED}));
            sw.setTrackTintList(new ColorStateList(new int[][]{{android.R.attr.state_checked}, {}}, new int[]{Color.rgb(36, 103, 89), Color.rgb(51, 66, 88)}));
        }
        sw.setOnCheckedChangeListener((view, on) -> changed.accept(on));
        return sw;
    }

    private LinearLayout card(String title, String subtitle) {
        return card(text(title, 15, TEXT, true), subtitle);
    }

    private LinearLayout card(View titleView, String subtitle) {
        LinearLayout card = new LinearLayout(this);
        card.setOrientation(LinearLayout.VERTICAL);
        card.setPadding(dp(12), dp(10), dp(12), dp(10));
        card.setBackground(round(CARD, 10));
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(-1, -2);
        params.setMargins(0, 0, 0, dp(8));
        card.setLayoutParams(params);
        if (titleView != null) card.addView(titleView);
        if (subtitle != null && !subtitle.isEmpty()) card.addView(text(subtitle, 11, MUTED, false));
        return card;
    }

    private TextView text(String value, int size, int color, boolean bold) {
        TextView view = new TextView(this);
        view.setText(value);
        view.setTextSize(size);
        view.setTextColor(color);
        view.setLineSpacing(0, 1.05f);
        view.setPadding(0, dp(2), 0, dp(2));
        if (bold) view.setTypeface(Typeface.DEFAULT, Typeface.BOLD);
        return view;
    }

    private Button btn(String label, boolean primary, Runnable action) {
        Button button = new Button(this);
        button.setText(label);
        button.setAllCaps(false);
        button.setTextSize(12);
        button.setTextColor(primary ? BG : TEXT);
        button.setMinHeight(dp(34));
        button.setMinimumHeight(dp(34));
        button.setPadding(dp(11), 0, dp(11), 0);
        GradientDrawable content = round(primary ? ACCENT : Color.rgb(31, 39, 49), 8);
        button.setBackground(new RippleDrawable(ColorStateList.valueOf(primary ? 0x55ffffff : 0x3368d5bd), content, round(Color.WHITE, 8)));
        button.setOnClickListener(view -> action.run());
        return button;
    }

    private void loadBranding() {
        try (java.io.InputStream stream = getAssets().open("branding/tokens.json")) {
            java.io.ByteArrayOutputStream output = new java.io.ByteArrayOutputStream();
            byte[] buffer = new byte[4096];
            int count;
            while ((count = stream.read(buffer)) != -1) output.write(buffer, 0, count);
            JSONObject colors = new JSONObject(output.toString("UTF-8")).getJSONObject("colors");
            BG = Color.parseColor(colors.getString("background"));
            CARD = Color.parseColor(colors.getString("surface"));
            TEXT = Color.parseColor(colors.getString("text"));
            MUTED = Color.parseColor(colors.getString("muted"));
            ACCENT = Color.parseColor(colors.getString("accent"));
            DANGER = Color.parseColor(colors.getString("danger"));
            CONTROL = Color.parseColor(colors.getString("control"));
            BORDER = Color.parseColor(colors.getString("border"));
            BLUE = Color.parseColor(colors.getString("accentBlue"));
        } catch (Exception error) {
            android.util.Log.w("Branding", "Using built-in colors", error);
        }
    }

    private Bitmap loadBitmap(String name) {
        try (java.io.InputStream stream = getAssets().open("branding/png/" + name + ".png")) {
            return BitmapFactory.decodeStream(stream);
        } catch (Exception error) {
            android.util.Log.w("Branding", "Could not load " + name, error);
            return null;
        }
    }

    private void setFooterIcon(Button button, String name, int tint) {
        Bitmap bitmap = loadBitmap(name);
        if (bitmap == null) return;
        BitmapDrawable icon = new BitmapDrawable(getResources(), bitmap);
        icon.setTint(tint);
        icon.setBounds(0, 0, dp(21), dp(21));
        button.setCompoundDrawables(null, icon, null, null);
        button.setCompoundDrawablePadding(dp(2));
    }

    private void styleTab(Button button, boolean active) {
        int color = active ? ACCENT : MUTED;
        button.setTextColor(color);
        GradientDrawable content = round(active ? Color.rgb(27, 37, 44) : Color.TRANSPARENT, 10);
        button.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), content, round(Color.WHITE, 10)));
        for (int i = 0; i < tabBtns.length; i++) {
            if (tabBtns[i] == button) setFooterIcon(button, ICONS[i], color);
        }
    }

    private LinearLayout row(View... views) {
        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        for (int i = 0; i < views.length; i++) {
            View view = views[i];
            LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(i == 0 ? 0 : -2, -2);
            if (i == 0) params.weight = 1;
            params.setMargins(dp(2), dp(2), dp(2), dp(2));
            row.addView(view, params);
        }
        return row;
    }

    private LinearLayout equalRow(View... views) {
        LinearLayout row = new LinearLayout(this);
        row.setGravity(Gravity.CENTER_VERTICAL);
        for (View view : views) {
            LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, -2, 1);
            params.setMargins(dp(2), dp(2), dp(2), dp(2));
            row.addView(view, params);
        }
        return row;
    }

    private void addMetric(LinearLayout row, String name, String value) {
        LinearLayout metric = new LinearLayout(this);
        metric.setOrientation(LinearLayout.VERTICAL);
        metric.setPadding(dp(7), dp(7), dp(7), dp(7));
        metric.setBackground(round(Color.rgb(19, 24, 31), 8));
        metric.addView(text(name, 10, MUTED, false));
        metric.addView(text(value, 14, TEXT, true));
        LinearLayout.LayoutParams params = new LinearLayout.LayoutParams(0, -2, 1);
        params.setMargins(dp(2), dp(4), dp(2), 0);
        row.addView(metric, params);
    }

    private GradientDrawable round(int color, int radius) {
        GradientDrawable shape = new GradientDrawable();
        shape.setColor(color);
        shape.setCornerRadius(dp(radius));
        return shape;
    }

    private View spacer(int height) {
        View view = new View(this);
        view.setLayoutParams(new LinearLayout.LayoutParams(1, dp(height)));
        return view;
    }

    private void ask(String title, String initial, int variation, Consumer<String> ok) {
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), Color.rgb(43, 53, 65));
        root.setBackground(bg);

        TextView titleView = text(title, 16, TEXT, true);
        titleView.setPadding(0, 0, 0, dp(14));
        root.addView(titleView);

        EditText input = new EditText(this);
        input.setInputType(variation == InputType.TYPE_CLASS_NUMBER ? variation : InputType.TYPE_CLASS_TEXT | variation);
        input.setText(initial);
        if (initial != null) {
            input.setSelection(initial.length());
        }
        input.setTextColor(TEXT);
        input.setHintTextColor(MUTED);
        input.setTextSize(14);
        int padH = dp(12), padV = dp(10);
        input.setPadding(padH, padV, padH, padV);
        input.setSingleLine(true);

        GradientDrawable inputBg = round(Color.rgb(31, 39, 49), 8);
        inputBg.setStroke(dp(1), Color.rgb(43, 53, 65));
        input.setBackground(inputBg);

        input.setOnFocusChangeListener((v, hasFocus) -> {
            inputBg.setStroke(dp(1), hasFocus ? ACCENT : Color.rgb(43, 53, 65));
        });

        LinearLayout.LayoutParams inputParams = new LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT);
        inputParams.setMargins(0, 0, 0, dp(18));
        root.addView(input, inputParams);

        LinearLayout buttons = new LinearLayout(this);
        buttons.setOrientation(LinearLayout.HORIZONTAL);
        buttons.setGravity(Gravity.END);

        Button cancelBtn = btn("Cancel", false, dialog::dismiss);
        Button saveBtn = btn("Save", true, () -> {
            String value = input.getText().toString().trim();
            if (!value.isEmpty() && !Ctl.connected) {
                toast("Daemon not connected");
            } else if (!value.isEmpty() || title.contains("proxy") || title.contains("DNS") || title.contains("TUN interface")) {
                dialog.dismiss();
                ok.accept(value);
            }
        });

        buttons.addView(cancelBtn);
        View btnSpacer = new View(this);
        buttons.addView(btnSpacer, new LinearLayout.LayoutParams(dp(8), 1));
        buttons.addView(saveBtn);
        root.addView(buttons);

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            int screenWidth = getResources().getDisplayMetrics().widthPixels;
            int dialogWidth = Math.min((int) (screenWidth * 0.90f), dp(400));
            dialog.getWindow().setLayout(dialogWidth, ViewGroup.LayoutParams.WRAP_CONTENT);
            dialog.getWindow().setSoftInputMode(WindowManager.LayoutParams.SOFT_INPUT_STATE_ALWAYS_VISIBLE);
        }

        input.setOnEditorActionListener((v, actionId, event) -> {
            saveBtn.performClick();
            return true;
        });

        dialog.show();
        input.requestFocus();
    }

    private void choose(String title, String[] items, IntConsumer pick) {
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), Color.rgb(43, 53, 65));
        root.setBackground(bg);

        TextView titleView = text(title, 16, TEXT, true);
        titleView.setPadding(0, 0, 0, dp(12));
        root.addView(titleView);

        ScrollView scroll = new ScrollView(this);
        LinearLayout itemsList = new LinearLayout(this);
        itemsList.setOrientation(LinearLayout.VERTICAL);

        for (int i = 0; i < items.length; i++) {
            final int index = i;
            TextView item = text(items[i], 14, TEXT, false);
            item.setPadding(dp(12), dp(10), dp(12), dp(10));
            GradientDrawable itemBg = round(Color.TRANSPARENT, 6);
            item.setBackground(new RippleDrawable(ColorStateList.valueOf(0x3368d5bd), itemBg, round(Color.WHITE, 6)));
            item.setClickable(true);
            item.setFocusable(true);
            item.setOnClickListener(v -> {
                dialog.dismiss();
                pick.accept(index);
            });
            itemsList.addView(item);
        }
        scroll.addView(itemsList);
        int listWidth = Math.min((int) (getResources().getDisplayMetrics().widthPixels * 0.90f), dp(380)) - dp(40);
        itemsList.measure(View.MeasureSpec.makeMeasureSpec(listWidth, View.MeasureSpec.EXACTLY), View.MeasureSpec.UNSPECIFIED);
        int listHeight = Math.min(itemsList.getMeasuredHeight(), (int) (getResources().getDisplayMetrics().heightPixels * 0.55f));
        LinearLayout.LayoutParams scrollParams = new LinearLayout.LayoutParams(LinearLayout.LayoutParams.MATCH_PARENT, listHeight);
        scrollParams.setMargins(0, 0, 0, dp(12));
        root.addView(scroll, scrollParams);

        LinearLayout buttons = new LinearLayout(this);
        buttons.setOrientation(LinearLayout.HORIZONTAL);
        buttons.setGravity(Gravity.END);
        Button cancelBtn = btn("Cancel", false, dialog::dismiss);
        buttons.addView(cancelBtn);
        root.addView(buttons);

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            int screenWidth = getResources().getDisplayMetrics().widthPixels;
            int dialogWidth = Math.min((int) (screenWidth * 0.90f), dp(380));
            dialog.getWindow().setLayout(dialogWidth, ViewGroup.LayoutParams.WRAP_CONTENT);
        }
        dialog.show();
    }

    private void confirm(String question, Runnable yes) {
        Dialog dialog = new Dialog(this);
        dialog.requestWindowFeature(Window.FEATURE_NO_TITLE);

        LinearLayout root = new LinearLayout(this);
        root.setOrientation(LinearLayout.VERTICAL);
        root.setPadding(dp(20), dp(18), dp(20), dp(18));
        GradientDrawable bg = round(CARD, 12);
        bg.setStroke(dp(1), Color.rgb(43, 53, 65));
        root.setBackground(bg);

        TextView msgView = text(question, 15, TEXT, false);
        msgView.setPadding(0, 0, 0, dp(18));
        root.addView(msgView);

        LinearLayout buttons = new LinearLayout(this);
        buttons.setOrientation(LinearLayout.HORIZONTAL);
        buttons.setGravity(Gravity.END);

        Button cancelBtn = btn("Cancel", false, dialog::dismiss);
        Button confirmBtn = btn("Confirm", true, () -> {
            dialog.dismiss();
            yes.run();
        });

        buttons.addView(cancelBtn);
        View btnSpacer = new View(this);
        buttons.addView(btnSpacer, new LinearLayout.LayoutParams(dp(8), 1));
        buttons.addView(confirmBtn);
        root.addView(buttons);

        dialog.setContentView(root);
        if (dialog.getWindow() != null) {
            dialog.getWindow().setBackgroundDrawable(new ColorDrawable(Color.TRANSPARENT));
            int screenWidth = getResources().getDisplayMetrics().widthPixels;
            int dialogWidth = Math.min((int) (screenWidth * 0.90f), dp(360));
            dialog.getWindow().setLayout(dialogWidth, ViewGroup.LayoutParams.WRAP_CONTENT);
        }
        dialog.show();
    }

    private void copy(String value) {
        copy("SOCKS5 address", value, "Copied to clipboard");
    }

    private void copy(String label, String value, String message) {
        ClipboardManager clipboard = getSystemService(ClipboardManager.class);
        clipboard.setPrimaryClip(ClipData.newPlainText(label, value));
        toast(message);
    }

    private static String tunSlot(JSONObject st) {
        JSONArray slots = st.optJSONArray("slots");
        String wanted = st.optString("tun_slot"), first = "";
        for (int i = 0; slots != null && i < slots.length(); i++) {
            JSONObject slot = slots.optJSONObject(i);
            if (!slot.optBoolean("enabled")) continue;
            if (first.isEmpty()) first = slot.optString("name");
            if (slot.optString("name").equals(wanted)) return wanted;
        }
        return first;
    }

    /** A dialable host for the SOCKS listeners: a wildcard bind is reached on loopback. */
    private static String connectHost(JSONObject st) {
        String address = st.optString("listen_addr");
        return address.isEmpty() || address.equals("0.0.0.0") || address.equals("::") ? "127.0.0.1" : address;
    }

    private static int freePort(JSONObject st) {
        int base = st.optInt("listen_port", 2084);
        List<Integer> used = new ArrayList<>();
        used.add(base); used.add(base + 1);
        used.add(st.optInt("entry_port"));
        JSONArray slots = st.optJSONArray("slots");
        for (int i = 0; slots != null && i < slots.length(); i++) {
            int port = slots.optJSONObject(i).optInt("port");
            used.add(port); used.add(port + 1);
        }
        for (int port = base + 2; port < 65535; port++) if (!used.contains(port) && !used.contains(port + 1)) return port;
        return 0;
    }

    private static boolean anyMain(JSONObject st) {
        JSONArray slots = st.optJSONArray("slots");
        for (int i = 0; slots != null && i < slots.length(); i++) {
            JSONArray mains = slots.optJSONObject(i).optJSONArray("mains");
            if (mains != null && mains.length() > 0) return true;
        }
        return false;
    }

    private static String proto(String value) {
        android.net.Uri uri = android.net.Uri.parse(value);
        String scheme = uri.getScheme() == null ? "" : uri.getScheme().toLowerCase(java.util.Locale.ROOT);
        if (scheme.equals("http") || scheme.equals("socks") || scheme.equals("socks5") || scheme.equals("socks5h")) {
            String tag = scheme.equals("http") ? "http" : "socks5";
            String userInfo = uri.getUserInfo();
            return userInfo != null && !userInfo.isEmpty() ? tag + " | auth" : tag;
        }
        String flow = uri.getQueryParameter("flow"), security = uri.getQueryParameter("security"), network = uri.getQueryParameter("type");
        String tag = flow != null && flow.contains("xtls-rprx-vision") ? "vision" : security == null || security.isEmpty() || security.equals("none") ? "plain" : security;
        return network == null || network.isEmpty() || network.equals("tcp") ? tag : tag + " | " + network;
    }

    private static String mainName(String value) {
        android.net.Uri uri = android.net.Uri.parse(value);
        String fragment = uri.getFragment();
        return fragment != null && !fragment.trim().isEmpty() ? fragment.trim() : uri.getHost();
    }

    private static String mainHost(String value) {
        android.net.Uri uri = android.net.Uri.parse(value);
        return uri.getHost() == null ? "Invalid exit URL" : uri.getHost() + (uri.getPort() > 0 ? ":" + uri.getPort() : "");
    }

    private static JSONArray copy(JSONArray value) {
        try { return new JSONArray(value == null ? "[]" : value.toString()); } catch (Exception e) { return new JSONArray(); }
    }

    private void fetching(String what) {
        Core.log("-> " + what + " ...");
        toast("Fetching... results appear in Activity");
    }

    private void failed(Exception error) {
        Core.log("Edit failed: " + error);
        toast("Edit failed: " + error.getMessage());
    }

    private void toast(String value) {
        Toast.makeText(this, value, Toast.LENGTH_SHORT).show();
    }

    private int dp(int value) {
        return Math.round(value * getResources().getDisplayMetrics().density);
    }
}
