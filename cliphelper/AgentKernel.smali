.class public LAgentKernel;
.super Ljava/lang/Object;
.implements Ljava/lang/Runnable;

.field private static sUi:Landroid/app/UiAutomation;
.field private static sThread:Landroid/os/HandlerThread;
.field private static sClip:Landroid/content/IClipboard;
.field private static sOut:Ljava/io/PrintStream;
.field private static sCtx:Landroid/content/Context;
.field private static sNM:Landroid/app/INotificationManager;
.field private static sIcon:Landroid/graphics/drawable/Icon;
.field private static sChanReady:Z

.method public constructor <init>()V
    .locals 0
    invoke-direct {p0}, Ljava/lang/Object;-><init>()V
    return-void
.end method

.method private static charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    .locals 1
    if-nez p0, :non_null
    const-string v0, ""
    return-object v0
    :non_null
    invoke-interface {p0}, Ljava/lang/CharSequence;->toString()Ljava/lang/String;
    move-result-object v0
    invoke-virtual {v0}, Ljava/lang/String;->trim()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static getClipboardService()Landroid/content/IClipboard;
    .locals 2
    sget-object v0, LAgentKernel;->sClip:Landroid/content/IClipboard;
    if-eqz v0, :init_clip
    return-object v0
    :init_clip
    const-string v0, "clipboard"
    invoke-static {v0}, Landroid/os/ServiceManager;->getService(Ljava/lang/String;)Landroid/os/IBinder;
    move-result-object v0
    if-nez v0, :got_binder
    new-instance v0, Ljava/lang/RuntimeException;
    const-string v1, "clipboard service unavailable"
    invoke-direct {v0, v1}, Ljava/lang/RuntimeException;-><init>(Ljava/lang/String;)V
    throw v0
    :got_binder
    invoke-static {v0}, Landroid/content/IClipboard$Stub;->asInterface(Landroid/os/IBinder;)Landroid/content/IClipboard;
    move-result-object v0
    sput-object v0, LAgentKernel;->sClip:Landroid/content/IClipboard;
    return-object v0
.end method

.method private static ensureUiAutomation()Landroid/app/UiAutomation;
    .locals 4
    sget-object v0, LAgentKernel;->sUi:Landroid/app/UiAutomation;
    if-eqz v0, :init_ui
    return-object v0

    :init_ui
    new-instance v0, Landroid/os/HandlerThread;
    const-string v1, "AgentUiThread"
    invoke-direct {v0, v1}, Landroid/os/HandlerThread;-><init>(Ljava/lang/String;)V
    invoke-virtual {v0}, Landroid/os/HandlerThread;->start()V
    sput-object v0, LAgentKernel;->sThread:Landroid/os/HandlerThread;

    invoke-virtual {v0}, Landroid/os/HandlerThread;->getLooper()Landroid/os/Looper;
    move-result-object v1
    new-instance v2, Landroid/app/UiAutomationConnection;
    invoke-direct {v2}, Landroid/app/UiAutomationConnection;-><init>()V
    new-instance v3, Landroid/app/UiAutomation;
    invoke-direct {v3, v1, v2}, Landroid/app/UiAutomation;-><init>(Landroid/os/Looper;Landroid/app/IUiAutomationConnection;)V
    invoke-virtual {v3}, Landroid/app/UiAutomation;->connect()V

    :try_flags
    invoke-virtual {v3}, Landroid/app/UiAutomation;->getServiceInfo()Landroid/accessibilityservice/AccessibilityServiceInfo;
    move-result-object v0
    if-eqz v0, :done_flags
    iget v1, v0, Landroid/accessibilityservice/AccessibilityServiceInfo;->flags:I
    # FLAG_RETRIEVE_INTERACTIVE_WINDOWS (0x40) | FLAG_REPORT_VIEW_IDS (0x10)
    or-int/lit8 v1, v1, 0x50
    iput v1, v0, Landroid/accessibilityservice/AccessibilityServiceInfo;->flags:I
    invoke-virtual {v3, v0}, Landroid/app/UiAutomation;->setServiceInfo(Landroid/accessibilityservice/AccessibilityServiceInfo;)V
    :try_end_flags
    .catch Ljava/lang/Throwable; {:try_flags .. :try_end_flags} :done_flags

    :done_flags
    sput-object v3, LAgentKernel;->sUi:Landroid/app/UiAutomation;
    return-object v3
.end method

.method private static shutdownUiAutomation()V
    .locals 3
    const/4 v2, 0x0
    sget-object v0, LAgentKernel;->sUi:Landroid/app/UiAutomation;
    if-eqz v0, :check_thread
    :try_disc
    invoke-virtual {v0}, Landroid/app/UiAutomation;->disconnect()V
    :try_end_disc
    .catch Ljava/lang/Throwable; {:try_disc .. :try_end_disc} :clear_ui
    :clear_ui
    sput-object v2, LAgentKernel;->sUi:Landroid/app/UiAutomation;

    :check_thread
    sget-object v1, LAgentKernel;->sThread:Landroid/os/HandlerThread;
    if-eqz v1, :done_shut
    :try_quit
    invoke-virtual {v1}, Landroid/os/HandlerThread;->quitSafely()Z
    :try_end_quit
    .catch Ljava/lang/Throwable; {:try_quit .. :try_end_quit} :clear_thread
    :clear_thread
    sput-object v2, LAgentKernel;->sThread:Landroid/os/HandlerThread;

    :done_shut
    invoke-static {}, Landroid/os/Looper;->getMainLooper()Landroid/os/Looper;
    move-result-object v0
    if-eqz v0, :ret_shut
    :try_main_quit
    invoke-virtual {v0}, Landroid/os/Looper;->quitSafely()V
    :try_end_main_quit
    .catch Ljava/lang/Throwable; {:try_main_quit .. :try_end_main_quit} :catch_main_quit
    goto :ret_shut
    :catch_main_quit
    move-exception v0
    :ret_shut
    return-void
.end method

.method private static findRootNode(Landroid/app/UiAutomation;)Landroid/view/accessibility/AccessibilityNodeInfo;
    .locals 9
    invoke-virtual {p0}, Landroid/app/UiAutomation;->getRootInActiveWindow()Landroid/view/accessibility/AccessibilityNodeInfo;
    move-result-object v0
    if-eqz v0, :try_windows
    return-object v0

    :try_windows
    invoke-virtual {p0}, Landroid/app/UiAutomation;->getWindows()Ljava/util/List;
    move-result-object v0
    if-nez v0, :scan_list
    const/4 v1, 0x0
    return-object v1

    :scan_list
    const/4 v1, 0x0
    const/4 v7, 0x0
    const/4 v8, 0x1
    invoke-interface {v0}, Ljava/util/List;->size()I
    move-result v2
    const/4 v3, 0x0

    :win_loop
    if-ge v3, v2, :win_done
    invoke-interface {v0, v3}, Ljava/util/List;->get(I)Ljava/lang/Object;
    move-result-object v4
    check-cast v4, Landroid/view/accessibility/AccessibilityWindowInfo;
    if-eqz v4, :win_next
    if-nez v7, :win_recycle

    :try_win_get
    invoke-virtual {v4}, Landroid/view/accessibility/AccessibilityWindowInfo;->getRoot()Landroid/view/accessibility/AccessibilityNodeInfo;
    move-result-object v5
    if-eqz v5, :win_recycle
    invoke-virtual {v4}, Landroid/view/accessibility/AccessibilityWindowInfo;->getType()I
    move-result v6
    :try_end_win_get
    .catch Ljava/lang/Throwable; {:try_win_get .. :try_end_win_get} :win_recycle

    if-ne v6, v8, :save_fallback
    move-object v7, v5
    if-eqz v1, :win_recycle
    :try_rec_fb
    invoke-virtual {v1}, Landroid/view/accessibility/AccessibilityNodeInfo;->recycle()V
    :try_end_rec_fb
    .catch Ljava/lang/Throwable; {:try_rec_fb .. :try_end_rec_fb} :clear_fb
    :clear_fb
    const/4 v1, 0x0
    goto :win_recycle

    :save_fallback
    if-nez v1, :rec_extra
    move-object v1, v5
    goto :win_recycle

    :rec_extra
    :try_rec_ex
    invoke-virtual {v5}, Landroid/view/accessibility/AccessibilityNodeInfo;->recycle()V
    :try_end_rec_ex
    .catch Ljava/lang/Throwable; {:try_rec_ex .. :try_end_rec_ex} :win_recycle

    :win_recycle
    :try_rec_w
    invoke-virtual {v4}, Landroid/view/accessibility/AccessibilityWindowInfo;->recycle()V
    :try_end_rec_w
    .catch Ljava/lang/Throwable; {:try_rec_w .. :try_end_rec_w} :win_next

    :win_next
    add-int/lit8 v3, v3, 0x1
    goto :win_loop

    :win_done
    if-eqz v7, :ret_fallback
    return-object v7

    :ret_fallback
    return-object v1
.end method

.method private static buildFlags(Landroid/view/accessibility/AccessibilityNodeInfo;)Ljava/lang/String;
    .locals 2
    new-instance v0, Ljava/lang/StringBuilder;
    invoke-direct {v0}, Ljava/lang/StringBuilder;-><init>()V

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isClickable()Z
    move-result v1
    if-eqz v1, :chk_long
    const/16 v1, 0x63
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_long
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isLongClickable()Z
    move-result v1
    if-eqz v1, :chk_scroll
    const/16 v1, 0x6c
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_scroll
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isScrollable()Z
    move-result v1
    if-eqz v1, :chk_edit
    const/16 v1, 0x73
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_edit
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isEditable()Z
    move-result v1
    if-eqz v1, :chk_checkable
    const/16 v1, 0x65
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_checkable
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isCheckable()Z
    move-result v1
    if-eqz v1, :chk_checked
    const/16 v1, 0x6b
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_checked
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isChecked()Z
    move-result v1
    if-eqz v1, :chk_selected
    const/16 v1, 0x78
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_selected
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isSelected()Z
    move-result v1
    if-eqz v1, :chk_focused
    const/16 v1, 0x53
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_focused
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isFocused()Z
    move-result v1
    if-eqz v1, :chk_pass
    const/16 v1, 0x66
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :chk_pass
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isPassword()Z
    move-result v1
    if-eqz v1, :done_flags
    const/16 v1, 0x70
    invoke-virtual {v0, v1}, Ljava/lang/StringBuilder;->append(C)Ljava/lang/StringBuilder;

    :done_flags
    invoke-virtual {v0}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static emitNode(Landroid/view/accessibility/AccessibilityNodeInfo;Landroid/graphics/Rect;ILorg/json/JSONArray;)V
    .locals 7
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Lorg/json/JSONException;
        }
    .end annotation

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getText()Ljava/lang/CharSequence;
    move-result-object v0
    invoke-static {v0}, LAgentKernel;->charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    move-result-object v0

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getContentDescription()Ljava/lang/CharSequence;
    move-result-object v1
    invoke-static {v1}, LAgentKernel;->charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    move-result-object v1

    invoke-static {p0}, LAgentKernel;->buildFlags(Landroid/view/accessibility/AccessibilityNodeInfo;)Ljava/lang/String;
    move-result-object v2

    invoke-virtual {v0}, Ljava/lang/String;->length()I
    move-result v3
    if-nez v3, :keep_node
    invoke-virtual {v1}, Ljava/lang/String;->length()I
    move-result v3
    if-nez v3, :keep_node
    invoke-virtual {v2}, Ljava/lang/String;->length()I
    move-result v3
    if-nez v3, :keep_node
    return-void

    :keep_node
    new-instance v3, Lorg/json/JSONObject;
    invoke-direct {v3}, Lorg/json/JSONObject;-><init>()V

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getClassName()Ljava/lang/CharSequence;
    move-result-object v4
    invoke-static {v4}, LAgentKernel;->charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    move-result-object v4
    const-string v5, "cls"
    invoke-virtual {v3, v5, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getPackageName()Ljava/lang/CharSequence;
    move-result-object v4
    invoke-static {v4}, LAgentKernel;->charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    move-result-object v4
    invoke-virtual {v4}, Ljava/lang/String;->length()I
    move-result v5
    if-lez v5, :skip_pkg
    const-string v5, "pkg"
    invoke-virtual {v3, v5, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_pkg
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getViewIdResourceName()Ljava/lang/String;
    move-result-object v4
    if-eqz v4, :skip_id
    invoke-virtual {v4}, Ljava/lang/String;->trim()Ljava/lang/String;
    move-result-object v4
    invoke-virtual {v4}, Ljava/lang/String;->length()I
    move-result v5
    if-lez v5, :skip_id
    const-string v5, "id"
    invoke-virtual {v3, v5, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_id
    invoke-virtual {v0}, Ljava/lang/String;->length()I
    move-result v4
    if-lez v4, :skip_t
    const-string v4, "t"
    invoke-virtual {v3, v4, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_t
    invoke-virtual {v1}, Ljava/lang/String;->length()I
    move-result v4
    if-lez v4, :skip_d
    const-string v4, "d"
    invoke-virtual {v3, v4, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_d
    invoke-virtual {v2}, Ljava/lang/String;->length()I
    move-result v4
    if-lez v4, :skip_f
    const-string v4, "f"
    invoke-virtual {v3, v4, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_f
    const-string v4, "dp"
    invoke-virtual {v3, v4, p2}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;

    new-instance v4, Lorg/json/JSONArray;
    invoke-direct {v4}, Lorg/json/JSONArray;-><init>()V
    iget v5, p1, Landroid/graphics/Rect;->left:I
    invoke-virtual {v4, v5}, Lorg/json/JSONArray;->put(I)Lorg/json/JSONArray;
    iget v5, p1, Landroid/graphics/Rect;->top:I
    invoke-virtual {v4, v5}, Lorg/json/JSONArray;->put(I)Lorg/json/JSONArray;
    iget v5, p1, Landroid/graphics/Rect;->right:I
    invoke-virtual {v4, v5}, Lorg/json/JSONArray;->put(I)Lorg/json/JSONArray;
    iget v5, p1, Landroid/graphics/Rect;->bottom:I
    invoke-virtual {v4, v5}, Lorg/json/JSONArray;->put(I)Lorg/json/JSONArray;
    const-string v5, "b"
    invoke-virtual {v3, v5, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    invoke-virtual {p3, v3}, Lorg/json/JSONArray;->put(Ljava/lang/Object;)Lorg/json/JSONArray;
    return-void
.end method

.method private static traverseNode(Landroid/view/accessibility/AccessibilityNodeInfo;II[ILorg/json/JSONArray;)V
    .locals 6
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Lorg/json/JSONException;
        }
    .end annotation

    if-nez p0, :non_null
    return-void

    :non_null
    if-le p1, p2, :check_limit
    const/4 v0, 0x1
    const/4 v1, 0x2
    aput v0, p3, v1
    return-void

    :check_limit
    const/4 v0, 0x0
    aget v1, p3, v0
    const/4 v2, 0x1
    aget v3, p3, v2
    if-lt v1, v3, :inc_count
    const/4 v3, 0x2
    aput v2, p3, v3
    return-void

    :inc_count
    add-int/lit8 v1, v1, 0x1
    aput v1, p3, v0

    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->isVisibleToUser()Z
    move-result v0
    if-nez v0, :is_vis
    const/4 v1, 0x2
    if-le p1, v1, :is_vis
    return-void

    :is_vis
    if-eqz v0, :walk_children
    new-instance v1, Landroid/graphics/Rect;
    invoke-direct {v1}, Landroid/graphics/Rect;-><init>()V
    invoke-virtual {p0, v1}, Landroid/view/accessibility/AccessibilityNodeInfo;->getBoundsInScreen(Landroid/graphics/Rect;)V
    iget v2, v1, Landroid/graphics/Rect;->right:I
    iget v3, v1, Landroid/graphics/Rect;->left:I
    if-le v2, v3, :walk_children
    iget v2, v1, Landroid/graphics/Rect;->bottom:I
    iget v3, v1, Landroid/graphics/Rect;->top:I
    if-le v2, v3, :walk_children
    invoke-static {p0, v1, p1, p4}, LAgentKernel;->emitNode(Landroid/view/accessibility/AccessibilityNodeInfo;Landroid/graphics/Rect;ILorg/json/JSONArray;)V

    :walk_children
    invoke-virtual {p0}, Landroid/view/accessibility/AccessibilityNodeInfo;->getChildCount()I
    move-result v0
    const/4 v1, 0x0
    add-int/lit8 v2, p1, 0x1

    :child_loop
    if-ge v1, v0, :done_walk
    const/4 v3, 0x0
    aget v3, p3, v3
    const/4 v4, 0x1
    aget v5, p3, v4
    if-lt v3, v5, :get_child
    const/4 v3, 0x2
    aput v4, p3, v3
    return-void

    :get_child
    const/4 v3, 0x0
    :try_child
    invoke-virtual {p0, v1}, Landroid/view/accessibility/AccessibilityNodeInfo;->getChild(I)Landroid/view/accessibility/AccessibilityNodeInfo;
    move-result-object v3
    if-eqz v3, :next_child
    invoke-static {v3, v2, p2, p3, p4}, LAgentKernel;->traverseNode(Landroid/view/accessibility/AccessibilityNodeInfo;II[ILorg/json/JSONArray;)V
    :try_end_child
    .catch Ljava/lang/Throwable; {:try_child .. :try_end_child} :recycle_child

    :recycle_child
    if-eqz v3, :next_child
    :try_rec
    invoke-virtual {v3}, Landroid/view/accessibility/AccessibilityNodeInfo;->recycle()V
    :try_end_rec
    .catch Ljava/lang/Throwable; {:try_rec .. :try_end_rec} :next_child

    :next_child
    add-int/lit8 v1, v1, 0x1
    goto :child_loop

    :done_walk
    return-void
.end method

.method private static handleUiTree(Lorg/json/JSONObject;)Ljava/lang/String;
    .locals 9
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Lorg/json/JSONException;
        }
    .end annotation

    const-string v0, "max_depth"
    const/16 v1, 0x24
    invoke-virtual {p0, v0, v1}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v0
    const/4 v1, 0x1
    if-ge v0, v1, :chk_depth_hi
    const/4 v0, 0x1
    :chk_depth_hi
    const/16 v2, 0x40
    if-le v0, v2, :depth_ok
    const/16 v0, 0x40

    :depth_ok
    const-string v2, "max_nodes"
    const/16 v3, 0x4b0
    invoke-virtual {p0, v2, v3}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v2
    const/16 v3, 0x32
    if-ge v2, v3, :chk_nodes_hi
    const/16 v2, 0x32
    :chk_nodes_hi
    const/16 v3, 0xbb8
    if-le v2, v3, :nodes_ok
    const/16 v2, 0xbb8

    :nodes_ok
    invoke-static {}, LAgentKernel;->ensureUiAutomation()Landroid/app/UiAutomation;
    move-result-object v4
    const/4 v5, 0x0
    const/4 v6, 0x6

    :retry_root
    invoke-static {v4}, LAgentKernel;->findRootNode(Landroid/app/UiAutomation;)Landroid/view/accessibility/AccessibilityNodeInfo;
    move-result-object v3
    if-nez v3, :root_ready
    add-int/lit8 v5, v5, 0x1
    if-ge v5, v6, :root_ready
    const-wide/16 v7, 0x1e
    :try_sleep_root
    invoke-static {v7, v8}, Ljava/lang/Thread;->sleep(J)V
    :try_end_sleep_root
    .catch Ljava/lang/Throwable; {:try_sleep_root .. :try_end_sleep_root} :catch_sleep_root
    goto :retry_root
    :catch_sleep_root
    move-exception v3
    goto :retry_root

    :root_ready
    new-instance v4, Lorg/json/JSONObject;
    invoke-direct {v4}, Lorg/json/JSONObject;-><init>()V
    invoke-static {v4}, LAgentKernel;->putDisplayInfo(Lorg/json/JSONObject;)V
    new-instance v5, Lorg/json/JSONArray;
    invoke-direct {v5}, Lorg/json/JSONArray;-><init>()V

    const-string v6, "ok"
    invoke-virtual {v4, v6, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;

    if-nez v3, :has_root
    const-string v0, "pkg"
    const-string v1, ""
    invoke-virtual {v4, v0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v0, "raw"
    const/4 v1, 0x0
    invoke-virtual {v4, v0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v0, "truncated"
    invoke-virtual {v4, v0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v0, "nodes"
    invoke-virtual {v4, v0, v5}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    invoke-virtual {v4}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0

    :has_root
    invoke-virtual {v3}, Landroid/view/accessibility/AccessibilityNodeInfo;->getPackageName()Ljava/lang/CharSequence;
    move-result-object v6
    invoke-static {v6}, LAgentKernel;->charSeqToStr(Ljava/lang/CharSequence;)Ljava/lang/String;
    move-result-object v6

    const/4 p0, 0x3
    new-array p0, p0, [I
    aput v2, p0, v1

    const/4 v2, 0x0
    :try_walk
    invoke-static {v3, v2, v0, p0, v5}, LAgentKernel;->traverseNode(Landroid/view/accessibility/AccessibilityNodeInfo;II[ILorg/json/JSONArray;)V
    :try_end_walk
    .catchall {:try_walk .. :try_end_walk} :finally_root

    :try_rec_root
    invoke-virtual {v3}, Landroid/view/accessibility/AccessibilityNodeInfo;->recycle()V
    :try_end_rec_root
    .catch Ljava/lang/Throwable; {:try_rec_root .. :try_end_rec_root} :build_res
    goto :build_res

    :finally_root
    move-exception v0
    :try_rec_fin
    invoke-virtual {v3}, Landroid/view/accessibility/AccessibilityNodeInfo;->recycle()V
    :try_end_rec_fin
    .catch Ljava/lang/Throwable; {:try_rec_fin .. :try_end_rec_fin} :rethrow_walk
    :rethrow_walk
    throw v0

    :build_res
    const-string v0, "pkg"
    invoke-virtual {v4, v0, v6}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v0, "raw"
    aget v2, p0, v2
    invoke-virtual {v4, v0, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const/4 v0, 0x2
    aget v0, p0, v0
    if-eqz v0, :not_trunc
    goto :put_trunc
    :not_trunc
    const/4 v1, 0x0
    :put_trunc
    const-string v0, "truncated"
    invoke-virtual {v4, v0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v0, "nodes"
    invoke-virtual {v4, v0, v5}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    invoke-virtual {v4}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static putDisplayInfo(Lorg/json/JSONObject;)V
    .locals 4
    :try_disp
    invoke-static {}, Landroid/hardware/display/DisplayManagerGlobal;->getInstance()Landroid/hardware/display/DisplayManagerGlobal;
    move-result-object v0
    if-eqz v0, :done_disp
    const/4 v1, 0x0
    invoke-virtual {v0, v1}, Landroid/hardware/display/DisplayManagerGlobal;->getRealDisplay(I)Landroid/view/Display;
    move-result-object v0
    if-eqz v0, :done_disp
    invoke-virtual {v0}, Landroid/view/Display;->getRotation()I
    move-result v1
    new-instance v2, Landroid/graphics/Point;
    invoke-direct {v2}, Landroid/graphics/Point;-><init>()V
    invoke-virtual {v0, v2}, Landroid/view/Display;->getRealSize(Landroid/graphics/Point;)V
    const-string v3, "w"
    iget v0, v2, Landroid/graphics/Point;->x:I
    invoke-virtual {p0, v3, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v3, "h"
    iget v0, v2, Landroid/graphics/Point;->y:I
    invoke-virtual {p0, v3, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v3, "rot"
    invoke-virtual {p0, v3, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    :try_end_disp
    .catch Ljava/lang/Throwable; {:try_disp .. :try_end_disp} :catch_disp
    goto :done_disp
    :catch_disp
    move-exception v0
    :done_disp
    return-void
.end method

.method private static handleClipSet(Lorg/json/JSONObject;)Ljava/lang/String;
    .locals 6
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Exception;
        }
    .end annotation

    invoke-static {}, LAgentKernel;->getClipboardService()Landroid/content/IClipboard;
    move-result-object v0
    const-string v1, "text"
    const-string v2, ""
    invoke-virtual {p0, v1, v2}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1
    const-string v2, "agent"
    invoke-static {v2, v1}, Landroid/content/ClipData;->newPlainText(Ljava/lang/CharSequence;Ljava/lang/CharSequence;)Landroid/content/ClipData;
    move-result-object v1

    const-string v2, "com.android.shell"
    const/4 v3, 0x0
    const/4 v4, 0x0
    const/4 v5, 0x0
    invoke-interface/range {v0 .. v5}, Landroid/content/IClipboard;->setPrimaryClip(Landroid/content/ClipData;Ljava/lang/String;Ljava/lang/String;II)V

    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v1, "ok"
    const/4 v2, 0x1
    invoke-virtual {v0, v1, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    invoke-virtual {v0}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static handleClipGet()Ljava/lang/String;
    .locals 5
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Exception;
        }
    .end annotation

    invoke-static {}, LAgentKernel;->getClipboardService()Landroid/content/IClipboard;
    move-result-object v0
    const-string v1, "com.android.shell"
    const/4 v2, 0x0
    const/4 v3, 0x0
    const/4 v4, 0x0
    invoke-interface {v0, v1, v2, v3, v4}, Landroid/content/IClipboard;->getPrimaryClip(Ljava/lang/String;Ljava/lang/String;II)Landroid/content/ClipData;
    move-result-object v0

    const-string v1, ""
    if-eqz v0, :done_text
    invoke-virtual {v0}, Landroid/content/ClipData;->getItemCount()I
    move-result v2
    if-lez v2, :done_text
    invoke-virtual {v0, v4}, Landroid/content/ClipData;->getItemAt(I)Landroid/content/ClipData$Item;
    move-result-object v0
    if-eqz v0, :done_text
    invoke-virtual {v0}, Landroid/content/ClipData$Item;->getText()Ljava/lang/CharSequence;
    move-result-object v0
    if-eqz v0, :done_text
    invoke-interface {v0}, Ljava/lang/CharSequence;->toString()Ljava/lang/String;
    move-result-object v1

    :done_text
    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v2, "ok"
    const/4 v3, 0x1
    invoke-virtual {v0, v2, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v2, "text"
    invoke-virtual {v0, v2, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    invoke-virtual {v0}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static makeErrorJson(Ljava/lang/String;)Ljava/lang/String;
    .locals 3
    :try_err
    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v1, "ok"
    const/4 v2, 0x0
    invoke-virtual {v0, v1, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v1, "error"
    if-nez p0, :non_null_err
    const-string p0, "unknown error"
    :non_null_err
    invoke-virtual {v0, v1, p0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    invoke-virtual {v0}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
    :try_end_err
    .catch Ljava/lang/Throwable; {:try_err .. :try_end_err} :fallback_err

    :fallback_err
    const-string v0, "{\"ok\":false,\"error\":\"json_encode_failed\"}"
    return-object v0
.end method

.method private static ensureNotifReady()V
    .locals 5
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Throwable;
        }
    .end annotation

    sget-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    if-nez v0, :nm_ok
    const-string v0, "notification"
    invoke-static {v0}, Landroid/os/ServiceManager;->getService(Ljava/lang/String;)Landroid/os/IBinder;
    move-result-object v0
    if-eqz v0, :nm_unavail
    invoke-static {v0}, Landroid/app/INotificationManager$Stub;->asInterface(Landroid/os/IBinder;)Landroid/app/INotificationManager;
    move-result-object v0
    sput-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    goto :nm_ok

    :nm_unavail
    new-instance v0, Ljava/lang/RuntimeException;
    const-string v1, "notification service unavailable"
    invoke-direct {v0, v1}, Ljava/lang/RuntimeException;-><init>(Ljava/lang/String;)V
    throw v0

    :nm_ok
    sget-object v0, LAgentKernel;->sCtx:Landroid/content/Context;
    if-nez v0, :ctx_ok
    invoke-static {}, Landroid/app/ActivityThread;->systemMain()Landroid/app/ActivityThread;
    move-result-object v0
    if-eqz v0, :ctx_ok
    invoke-virtual {v0}, Landroid/app/ActivityThread;->getSystemContext()Landroid/app/ContextImpl;
    move-result-object v0
    if-eqz v0, :ctx_ok
    :try_pkg_ctx
    const-string v1, "com.android.shell"
    const/4 v2, 0x0
    invoke-virtual {v0, v1, v2}, Landroid/content/Context;->createPackageContext(Ljava/lang/String;I)Landroid/content/Context;
    move-result-object v1
    if-eqz v1, :use_sys_ctx
    move-object v0, v1
    :try_end_pkg_ctx
    .catch Ljava/lang/Throwable; {:try_pkg_ctx .. :try_end_pkg_ctx} :use_sys_ctx
    :use_sys_ctx
    sput-object v0, LAgentKernel;->sCtx:Landroid/content/Context;

    :ctx_ok
    sget-object v0, LAgentKernel;->sIcon:Landroid/graphics/drawable/Icon;
    if-nez v0, :icon_ok
    sget-object v0, Landroid/graphics/Bitmap$Config;->ARGB_8888:Landroid/graphics/Bitmap$Config;
    const/16 v1, 0x30
    invoke-static {v1, v1, v0}, Landroid/graphics/Bitmap;->createBitmap(IILandroid/graphics/Bitmap$Config;)Landroid/graphics/Bitmap;
    move-result-object v0
    const v1, -0xf15a17
    invoke-virtual {v0, v1}, Landroid/graphics/Bitmap;->eraseColor(I)V
    invoke-static {v0}, Landroid/graphics/drawable/Icon;->createWithBitmap(Landroid/graphics/Bitmap;)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIcon:Landroid/graphics/drawable/Icon;

    :icon_ok
    sget-boolean v0, LAgentKernel;->sChanReady:Z
    if-nez v0, :done_ready
    sget-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    new-instance v1, Landroid/app/NotificationChannel;
    const-string v2, "agent_live_progress"
    const-string v3, "Agent 实时进度"
    const/4 v4, 0x3
    invoke-direct {v1, v2, v3, v4}, Landroid/app/NotificationChannel;-><init>(Ljava/lang/String;Ljava/lang/CharSequence;I)V
    const/4 v2, 0x0
    invoke-virtual {v1, v2, v2}, Landroid/app/NotificationChannel;->setSound(Landroid/net/Uri;Landroid/media/AudioAttributes;)V
    const/4 v2, 0x0
    invoke-virtual {v1, v2}, Landroid/app/NotificationChannel;->enableVibration(Z)V
    invoke-virtual {v1, v2}, Landroid/app/NotificationChannel;->setShowBadge(Z)V

    invoke-static {v1}, Ljava/util/Collections;->singletonList(Ljava/lang/Object;)Ljava/util/List;
    move-result-object v1
    new-instance v2, Landroid/content/pm/ParceledListSlice;
    invoke-direct {v2, v1}, Landroid/content/pm/ParceledListSlice;-><init>(Ljava/util/List;)V
    const-string v1, "com.android.shell"
    invoke-interface {v0, v1, v2}, Landroid/app/INotificationManager;->createNotificationChannels(Ljava/lang/String;Landroid/content/pm/ParceledListSlice;)V
    const/4 v0, 0x1
    sput-boolean v0, LAgentKernel;->sChanReady:Z

    :done_ready
    return-void
.end method

.method private static buildFocusParamJson(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    .locals 6
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Lorg/json/JSONException;
        }
    .end annotation

    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V

    new-instance v1, Lorg/json/JSONObject;
    invoke-direct {v1}, Lorg/json/JSONObject;-><init>()V
    const-string v2, "business"
    const-string v3, "agent_live"
    invoke-virtual {v1, v2, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v2, "protocol"
    const/4 v3, 0x1
    invoke-virtual {v1, v2, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v2, "ticker"
    invoke-virtual {v1, v2, p2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v2, "enableFloat"
    const/4 v4, 0x0
    invoke-virtual {v1, v2, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v2, "updatable"
    invoke-virtual {v1, v2, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v2, "isShowNotification"
    invoke-virtual {v1, v2, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;

    new-instance v2, Lorg/json/JSONObject;
    invoke-direct {v2}, Lorg/json/JSONObject;-><init>()V
    const-string v4, "title"
    invoke-virtual {v2, v4, p0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v4, "content"
    invoke-virtual {v2, v4, p1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v4, "type"
    const/4 v5, 0x2
    invoke-virtual {v2, v4, v5}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v4, "baseInfo"
    invoke-virtual {v1, v4, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v2, Lorg/json/JSONObject;
    invoke-direct {v2}, Lorg/json/JSONObject;-><init>()V
    const-string v4, "title"
    invoke-virtual {v2, v4, p2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v4, "content"
    invoke-virtual {v2, v4, p3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v4, Lorg/json/JSONObject;
    invoke-direct {v4}, Lorg/json/JSONObject;-><init>()V
    const-string v5, "type"
    invoke-virtual {v4, v5, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v5, "textInfo"
    invoke-virtual {v4, v5, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v2, Lorg/json/JSONObject;
    invoke-direct {v2}, Lorg/json/JSONObject;-><init>()V
    const-string v5, "imageTextInfoLeft"
    invoke-virtual {v2, v5, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v4, Lorg/json/JSONObject;
    invoke-direct {v4}, Lorg/json/JSONObject;-><init>()V
    const-string v5, "title"
    invoke-virtual {v4, v5, p2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v5, Lorg/json/JSONObject;
    invoke-direct {v5}, Lorg/json/JSONObject;-><init>()V
    const-string p0, "combinePicInfo"
    invoke-virtual {v5, p0, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v4, Lorg/json/JSONObject;
    invoke-direct {v4}, Lorg/json/JSONObject;-><init>()V
    const-string p0, "islandProperty"
    invoke-virtual {v4, p0, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string p0, "bigIslandArea"
    invoke-virtual {v4, p0, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string p0, "smallIslandArea"
    invoke-virtual {v4, p0, v5}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    const-string p0, "param_island"
    invoke-virtual {v1, p0, v4}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string p0, "param_v2"
    invoke-virtual {v0, p0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    invoke-virtual {v0}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static handleNotifyProgress(Lorg/json/JSONObject;)Ljava/lang/String;
    .locals 13
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Throwable;
        }
    .end annotation

    invoke-static {}, LAgentKernel;->ensureNotifReady()V
    sget-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    sget-object v1, LAgentKernel;->sCtx:Landroid/content/Context;
    sget-object v2, LAgentKernel;->sIcon:Landroid/graphics/drawable/Icon;

    const-string v3, "tag"
    const-string v4, "agent_live"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v7

    const-string v3, "id"
    const/16 v4, 0x26ac
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v8

    const-string v3, "title"
    const-string v4, "🤖 Agent 执行中"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v9

    const-string v3, "text"
    const-string v4, ""
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v10

    const-string v3, "short_text"
    invoke-virtual {p0, v3, v9}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v11

    const-string v3, "sub_text"
    const-string v4, "Agent"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v12

    new-instance v3, Landroid/app/Notification$Builder;
    const-string v4, "agent_live_progress"
    invoke-direct {v3, v1, v4}, Landroid/app/Notification$Builder;-><init>(Landroid/content/Context;Ljava/lang/String;)V
    invoke-virtual {v3, v2}, Landroid/app/Notification$Builder;->setSmallIcon(Landroid/graphics/drawable/Icon;)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v9}, Landroid/app/Notification$Builder;->setContentTitle(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v10}, Landroid/app/Notification$Builder;->setContentText(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;

    const/4 v4, 0x1
    const-string v5, "ongoing"
    invoke-virtual {p0, v5, v4}, Lorg/json/JSONObject;->optBoolean(Ljava/lang/String;Z)Z
    move-result v5
    invoke-virtual {v3, v5}, Landroid/app/Notification$Builder;->setOngoing(Z)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setOnlyAlertOnce(Z)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setShowWhen(Z)Landroid/app/Notification$Builder;
    const-wide v5, 0x2bf20L
    invoke-virtual {v3, v5, v6}, Landroid/app/Notification$Builder;->setTimeoutAfter(J)Landroid/app/Notification$Builder;

    const-string v5, "step"
    const/4 v6, 0x0
    invoke-virtual {p0, v5, v6}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v5
    const-string v1, "max"
    invoke-virtual {p0, v1, v6}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v1
    if-lez v1, :skip_prog
    invoke-virtual {v3, v1, v5, v6}, Landroid/app/Notification$Builder;->setProgress(IIZ)Landroid/app/Notification$Builder;

    :skip_prog
    const-string v1, "focus"
    invoke-virtual {p0, v1, v4}, Lorg/json/JSONObject;->optBoolean(Ljava/lang/String;Z)Z
    move-result v1
    if-eqz v1, :skip_focus
    new-instance v5, Landroid/os/Bundle;
    invoke-direct {v5}, Landroid/os/Bundle;-><init>()V
    const-string v1, "miui.focus.enable"
    invoke-virtual {v5, v1, v4}, Landroid/os/Bundle;->putBoolean(Ljava/lang/String;Z)V
    const-string v1, "miui.focus.ticker"
    invoke-virtual {v5, v1, v11}, Landroid/os/Bundle;->putString(Ljava/lang/String;Ljava/lang/String;)V
    invoke-static {v9, v10, v11, v12}, LAgentKernel;->buildFocusParamJson(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1
    const-string v2, "miui.focus.param"
    invoke-virtual {v5, v2, v1}, Landroid/os/Bundle;->putString(Ljava/lang/String;Ljava/lang/String;)V
    invoke-virtual {v3, v5}, Landroid/app/Notification$Builder;->addExtras(Landroid/os/Bundle;)Landroid/app/Notification$Builder;

    :skip_focus
    invoke-virtual {v3}, Landroid/app/Notification$Builder;->build()Landroid/app/Notification;
    move-result-object v5

    const-string v1, "com.android.shell"
    const-string v2, "com.android.shell"
    move-object v3, v7
    move v4, v8
    invoke-interface/range {v0 .. v6}, Landroid/app/INotificationManager;->enqueueNotificationWithTag(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;ILandroid/app/Notification;I)V
    const-string v0, "{\"ok\":true}"
    return-object v0
.end method

.method private static handleNotifyCancel(Lorg/json/JSONObject;)Ljava/lang/String;
    .locals 6
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Throwable;
        }
    .end annotation

    invoke-static {}, LAgentKernel;->ensureNotifReady()V
    sget-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    const-string v1, "tag"
    const-string v2, "agent_live"
    invoke-virtual {p0, v1, v2}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v3
    const-string v1, "id"
    const/16 v2, 0x26ac
    invoke-virtual {p0, v1, v2}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v4
    const-string v1, "com.android.shell"
    const-string v2, "com.android.shell"
    const/4 v5, 0x0
    invoke-interface/range {v0 .. v5}, Landroid/app/INotificationManager;->cancelNotificationWithTag(Ljava/lang/String;Ljava/lang/String;Ljava/lang/String;II)V
    const-string v0, "{\"ok\":true}"
    return-object v0
.end method

.method private static stdioLoop()V
    .locals 5
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Exception;
        }
    .end annotation

    sget-object v0, LAgentKernel;->sOut:Ljava/io/PrintStream;
    if-nez v0, :out_ready
    sget-object v0, Ljava/lang/System;->out:Ljava/io/PrintStream;
    sput-object v0, LAgentKernel;->sOut:Ljava/io/PrintStream;
    sget-object v1, Ljava/lang/System;->err:Ljava/io/PrintStream;
    invoke-static {v1}, Ljava/lang/System;->setOut(Ljava/io/PrintStream;)V

    :out_ready
    new-instance v0, Ljava/io/BufferedReader;
    new-instance v1, Ljava/io/InputStreamReader;
    sget-object v2, Ljava/lang/System;->in:Ljava/io/InputStream;
    const-string v3, "UTF-8"
    invoke-direct {v1, v2, v3}, Ljava/io/InputStreamReader;-><init>(Ljava/io/InputStream;Ljava/lang/String;)V
    invoke-direct {v0, v1}, Ljava/io/BufferedReader;-><init>(Ljava/io/Reader;)V

    :read_loop
    invoke-virtual {v0}, Ljava/io/BufferedReader;->readLine()Ljava/lang/String;
    move-result-object v1
    if-nez v1, :got_line
    invoke-static {}, LAgentKernel;->shutdownUiAutomation()V
    return-void

    :got_line
    invoke-virtual {v1}, Ljava/lang/String;->trim()Ljava/lang/String;
    move-result-object v1
    invoke-virtual {v1}, Ljava/lang/String;->length()I
    move-result v2
    if-nez v2, :parse_req
    goto :read_loop

    :parse_req
    const/4 v2, 0x0
    :try_req
    new-instance v3, Lorg/json/JSONObject;
    invoke-direct {v3, v1}, Lorg/json/JSONObject;-><init>(Ljava/lang/String;)V
    const-string v1, "cmd"
    const-string v4, ""
    invoke-virtual {v3, v1, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1

    const-string v4, "ping"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_ui_tree
    const-string v1, "{\"ok\":true,\"pong\":true}"
    goto :send_resp

    :chk_ui_tree
    const-string v4, "ui_tree"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_clip_get
    invoke-static {v3}, LAgentKernel;->handleUiTree(Lorg/json/JSONObject;)Ljava/lang/String;
    move-result-object v1
    goto :send_resp

    :chk_clip_get
    const-string v4, "clip_get"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_clip_set
    invoke-static {}, LAgentKernel;->handleClipGet()Ljava/lang/String;
    move-result-object v1
    goto :send_resp

    :chk_clip_set
    const-string v4, "clip_set"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_notify_prog
    invoke-static {v3}, LAgentKernel;->handleClipSet(Lorg/json/JSONObject;)Ljava/lang/String;
    move-result-object v1
    goto :send_resp

    :chk_notify_prog
    const-string v4, "notify_progress"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_notify_cancel
    invoke-static {v3}, LAgentKernel;->handleNotifyProgress(Lorg/json/JSONObject;)Ljava/lang/String;
    move-result-object v1
    goto :send_resp

    :chk_notify_cancel
    const-string v4, "notify_cancel"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :chk_quit
    invoke-static {v3}, LAgentKernel;->handleNotifyCancel(Lorg/json/JSONObject;)Ljava/lang/String;
    move-result-object v1
    goto :send_resp

    :chk_quit
    const-string v4, "quit"
    invoke-virtual {v4, v1}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :unknown_cmd
    const/4 v2, 0x1
    const-string v1, "{\"ok\":true}"
    goto :send_resp

    :unknown_cmd
    new-instance v3, Ljava/lang/StringBuilder;
    invoke-direct {v3}, Ljava/lang/StringBuilder;-><init>()V
    const-string v4, "unknown cmd: "
    invoke-virtual {v3, v4}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;
    invoke-virtual {v3, v1}, Ljava/lang/StringBuilder;->append(Ljava/lang/String;)Ljava/lang/StringBuilder;
    invoke-virtual {v3}, Ljava/lang/StringBuilder;->toString()Ljava/lang/String;
    move-result-object v1
    invoke-static {v1}, LAgentKernel;->makeErrorJson(Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1
    :try_end_req
    .catch Ljava/lang/Throwable; {:try_req .. :try_end_req} :catch_req
    goto :send_resp

    :catch_req
    move-exception v3
    invoke-virtual {v3}, Ljava/lang/Throwable;->toString()Ljava/lang/String;
    move-result-object v1
    invoke-static {v1}, LAgentKernel;->makeErrorJson(Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1

    :send_resp
    const/16 v3, 0xa
    const/16 v4, 0x20
    invoke-virtual {v1, v3, v4}, Ljava/lang/String;->replace(CC)Ljava/lang/String;
    move-result-object v1
    const/16 v3, 0xd
    invoke-virtual {v1, v3, v4}, Ljava/lang/String;->replace(CC)Ljava/lang/String;
    move-result-object v1

    sget-object v3, LAgentKernel;->sOut:Ljava/io/PrintStream;
    if-nez v3, :use_sout
    sget-object v3, Ljava/lang/System;->out:Ljava/io/PrintStream;
    :use_sout
    invoke-virtual {v3, v1}, Ljava/io/PrintStream;->println(Ljava/lang/String;)V
    invoke-virtual {v3}, Ljava/io/PrintStream;->flush()V

    if-eqz v2, :read_loop
    invoke-static {}, LAgentKernel;->shutdownUiAutomation()V
    return-void
.end method

.method public run()V
    .locals 2
    :try_loop
    invoke-static {}, LAgentKernel;->stdioLoop()V
    :try_end_loop
    .catch Ljava/lang/Throwable; {:try_loop .. :try_end_loop} :catch_loop
    return-void

    :catch_loop
    move-exception v0
    sget-object v1, Ljava/lang/System;->err:Ljava/io/PrintStream;
    invoke-virtual {v0, v1}, Ljava/lang/Throwable;->printStackTrace(Ljava/io/PrintStream;)V
    invoke-static {}, LAgentKernel;->shutdownUiAutomation()V
    return-void
.end method

.method public static main([Ljava/lang/String;)V
    .locals 3
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Exception;
        }
    .end annotation

    invoke-static {}, Landroid/os/Looper;->getMainLooper()Landroid/os/Looper;
    move-result-object v0
    if-nez v0, :has_looper
    invoke-static {}, Landroid/os/Looper;->prepareMainLooper()V

    :has_looper
    new-instance v0, LAgentKernel;
    invoke-direct {v0}, LAgentKernel;-><init>()V
    new-instance v1, Ljava/lang/Thread;
    const-string v2, "AgentKernelLoop"
    invoke-direct {v1, v0, v2}, Ljava/lang/Thread;-><init>(Ljava/lang/Runnable;Ljava/lang/String;)V
    invoke-virtual {v1}, Ljava/lang/Thread;->start()V

    invoke-static {}, Landroid/os/Looper;->loop()V
    return-void
.end method
