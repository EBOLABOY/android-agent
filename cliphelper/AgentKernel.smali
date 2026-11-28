.class public LAgentKernel;
.super Ljava/lang/Object;
.implements Ljava/lang/Runnable;

.field private static sUi:Landroid/app/UiAutomation;
.field private static sThread:Landroid/os/HandlerThread;
.field private static sClip:Landroid/content/IClipboard;
.field private static sOut:Ljava/io/PrintStream;
.field private static sCtx:Landroid/content/Context;
.field private static sNM:Landroid/app/INotificationManager;
.field private static sIconThinking:Landroid/graphics/drawable/Icon;
.field private static sIconActing:Landroid/graphics/drawable/Icon;
.field private static sIconWorkflow:Landroid/graphics/drawable/Icon;
.field private static sIconConfirm:Landroid/graphics/drawable/Icon;
.field private static sIconDone:Landroid/graphics/drawable/Icon;
.field private static sIconStopped:Landroid/graphics/drawable/Icon;
.field private static sIconError:Landroid/graphics/drawable/Icon;
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
    sget-object v0, LAgentKernel;->sCtx:Landroid/content/Context;
    if-eqz v0, :ctx_unavail

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

    :ctx_unavail
    new-instance v0, Ljava/lang/RuntimeException;
    const-string v1, "notification context unavailable"
    invoke-direct {v0, v1}, Ljava/lang/RuntimeException;-><init>(Ljava/lang/String;)V
    throw v0
.end method

.method private static renderStateIcon(I)Landroid/graphics/drawable/Icon;
    .locals 9

    sget-object v0, Landroid/graphics/Bitmap$Config;->ARGB_8888:Landroid/graphics/Bitmap$Config;
    const/16 v1, 0x60
    invoke-static {v1, v1, v0}, Landroid/graphics/Bitmap;->createBitmap(IILandroid/graphics/Bitmap$Config;)Landroid/graphics/Bitmap;
    move-result-object v0

    new-instance v3, Landroid/graphics/Canvas;
    invoke-direct {v3, v0}, Landroid/graphics/Canvas;-><init>(Landroid/graphics/Bitmap;)V

    new-instance v8, Landroid/graphics/Paint;
    const/4 v1, 0x1
    invoke-direct {v8, v1}, Landroid/graphics/Paint;-><init>(I)V
    sget-object v1, Landroid/graphics/Paint$Cap;->ROUND:Landroid/graphics/Paint$Cap;
    invoke-virtual {v8, v1}, Landroid/graphics/Paint;->setStrokeCap(Landroid/graphics/Paint$Cap;)V
    sget-object v1, Landroid/graphics/Paint$Join;->ROUND:Landroid/graphics/Paint$Join;
    invoke-virtual {v8, v1}, Landroid/graphics/Paint;->setStrokeJoin(Landroid/graphics/Paint$Join;)V

    const/4 v4, 0x1
    if-eq p0, v4, :pal_acting
    const/4 v4, 0x2
    if-eq p0, v4, :pal_workflow
    const/4 v4, 0x3
    if-eq p0, v4, :pal_confirm
    const/4 v4, 0x4
    if-eq p0, v4, :pal_done
    const/4 v4, 0x5
    if-eq p0, v4, :pal_rose
    const/4 v4, 0x6
    if-eq p0, v4, :pal_rose

    const v1, 0x226366f1
    const v2, -0x9c990f
    const v7, -0x7e7308
    goto :draw_bg

    :pal_acting
    const v1, 0x2210b981
    const v2, -0xef467f
    const v7, -0xcb2c67
    goto :draw_bg

    :pal_workflow
    const v1, 0x2206b6d4
    const v2, -0xf9492c
    const v7, -0xdd2c12
    goto :draw_bg

    :pal_confirm
    const v1, 0x22f59e0b
    const v2, -0xa61f5
    const v7, -0x440dc
    goto :draw_bg

    :pal_done
    const v1, 0x2222c55e
    const v2, -0xdd3aa2
    const v7, -0xb52180
    goto :draw_bg

    :pal_rose
    const v1, 0x22ef4444
    const v2, -0x10bbbc
    const v7, -0x78e8f

    :draw_bg
    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    invoke-virtual {v8, v1}, Landroid/graphics/Paint;->setColor(I)V
    new-instance v4, Landroid/graphics/RectF;
    const/high16 v5, 0x41200000
    const/high16 v6, 0x42ac0000
    invoke-direct {v4, v5, v5, v6, v6}, Landroid/graphics/RectF;-><init>(FFFF)V
    const/high16 v5, 0x41d00000
    invoke-virtual {v3, v4, v5, v5, v8}, Landroid/graphics/Canvas;->drawRoundRect(Landroid/graphics/RectF;FFLandroid/graphics/Paint;)V

    sget-object v4, Landroid/graphics/Paint$Style;->STROKE:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    const/high16 v4, 0x40f00000
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStrokeWidth(F)V
    invoke-virtual {v8, v2}, Landroid/graphics/Paint;->setColor(I)V

    const/4 v4, 0x3
    if-ne p0, v4, :ring_circle
    new-instance v4, Landroid/graphics/RectF;
    const/high16 v5, 0x41800000
    const/high16 v6, 0x42a00000
    invoke-direct {v4, v5, v5, v6, v6}, Landroid/graphics/RectF;-><init>(FFFF)V
    const/high16 v5, 0x41a00000
    invoke-virtual {v3, v4, v5, v5, v8}, Landroid/graphics/Canvas;->drawRoundRect(Landroid/graphics/RectF;FFLandroid/graphics/Paint;)V
    goto :draw_glyph

    :ring_circle
    const/high16 v4, 0x42400000
    const/high16 v5, 0x42000000
    invoke-virtual {v3, v4, v4, v5, v8}, Landroid/graphics/Canvas;->drawCircle(FFFLandroid/graphics/Paint;)V

    :draw_glyph
    invoke-virtual {v8, v7}, Landroid/graphics/Paint;->setColor(I)V

    const/4 v4, 0x1
    if-eq p0, v4, :glyph_acting
    const/4 v4, 0x2
    if-eq p0, v4, :glyph_workflow
    const/4 v4, 0x3
    if-eq p0, v4, :glyph_confirm
    const/4 v4, 0x4
    if-eq p0, v4, :glyph_done
    const/4 v4, 0x5
    if-eq p0, v4, :glyph_stopped
    const/4 v4, 0x6
    if-eq p0, v4, :glyph_error

    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    const/high16 v4, 0x42400000
    const/high16 v5, 0x41300000
    invoke-virtual {v3, v4, v4, v5, v8}, Landroid/graphics/Canvas;->drawCircle(FFFLandroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_acting
    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    new-instance v4, Landroid/graphics/Path;
    invoke-direct {v4}, Landroid/graphics/Path;-><init>()V
    const/high16 v5, 0x42240000
    const/high16 v6, 0x42080000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->moveTo(FF)V
    const/high16 v6, 0x42780000
    const/high16 v7, 0x42400000
    invoke-virtual {v4, v6, v7}, Landroid/graphics/Path;->lineTo(FF)V
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    invoke-virtual {v4}, Landroid/graphics/Path;->close()V
    invoke-virtual {v3, v4, v8}, Landroid/graphics/Canvas;->drawPath(Landroid/graphics/Path;Landroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_workflow
    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    new-instance v4, Landroid/graphics/Path;
    invoke-direct {v4}, Landroid/graphics/Path;-><init>()V
    const/high16 v5, 0x42500000
    const/high16 v6, 0x41e00000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->moveTo(FF)V
    const/high16 v5, 0x42140000
    const/high16 v6, 0x42480000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    const/high16 v5, 0x42400000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    const/high16 v5, 0x42300000
    const/high16 v6, 0x42880000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    const/high16 v5, 0x426c0000
    const/high16 v6, 0x42380000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    const/high16 v5, 0x42400000
    invoke-virtual {v4, v5, v6}, Landroid/graphics/Path;->lineTo(FF)V
    invoke-virtual {v4}, Landroid/graphics/Path;->close()V
    invoke-virtual {v3, v4, v8}, Landroid/graphics/Canvas;->drawPath(Landroid/graphics/Path;Landroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_confirm
    const/high16 v4, 0x41080000
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStrokeWidth(F)V
    const/high16 v4, 0x42400000
    const/high16 v5, 0x41f80000
    const/high16 v6, 0x42400000
    const/high16 v7, 0x42480000
    invoke-virtual/range {v3 .. v8}, Landroid/graphics/Canvas;->drawLine(FFFFLandroid/graphics/Paint;)V
    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    const/high16 v4, 0x42400000
    const/high16 v5, 0x427c0000
    const/high16 v6, 0x40a00000
    invoke-virtual {v3, v4, v5, v6, v8}, Landroid/graphics/Canvas;->drawCircle(FFFLandroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_done
    const/high16 v4, 0x41080000
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStrokeWidth(F)V
    const/high16 v4, 0x42080000
    const/high16 v5, 0x42440000
    const/high16 v6, 0x42300000
    const/high16 v7, 0x426c0000
    invoke-virtual/range {v3 .. v8}, Landroid/graphics/Canvas;->drawLine(FFFFLandroid/graphics/Paint;)V
    const/high16 v4, 0x42300000
    const/high16 v5, 0x426c0000
    const/high16 v6, 0x427c0000
    const/high16 v7, 0x42180000
    invoke-virtual/range {v3 .. v8}, Landroid/graphics/Canvas;->drawLine(FFFFLandroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_stopped
    sget-object v4, Landroid/graphics/Paint$Style;->FILL:Landroid/graphics/Paint$Style;
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStyle(Landroid/graphics/Paint$Style;)V
    new-instance v4, Landroid/graphics/RectF;
    const/high16 v5, 0x42100000
    const/high16 v6, 0x42700000
    invoke-direct {v4, v5, v5, v6, v6}, Landroid/graphics/RectF;-><init>(FFFF)V
    const/high16 v5, 0x40a00000
    invoke-virtual {v3, v4, v5, v5, v8}, Landroid/graphics/Canvas;->drawRoundRect(Landroid/graphics/RectF;FFLandroid/graphics/Paint;)V
    goto :finish_icon

    :glyph_error
    const/high16 v4, 0x41000000
    invoke-virtual {v8, v4}, Landroid/graphics/Paint;->setStrokeWidth(F)V
    const/high16 v4, 0x42140000
    const/high16 v5, 0x42140000
    const/high16 v6, 0x426c0000
    const/high16 v7, 0x426c0000
    invoke-virtual/range {v3 .. v8}, Landroid/graphics/Canvas;->drawLine(FFFFLandroid/graphics/Paint;)V
    const/high16 v4, 0x426c0000
    const/high16 v5, 0x42140000
    const/high16 v6, 0x42140000
    const/high16 v7, 0x426c0000
    invoke-virtual/range {v3 .. v8}, Landroid/graphics/Canvas;->drawLine(FFFFLandroid/graphics/Paint;)V

    :finish_icon
    invoke-static {v0}, Landroid/graphics/drawable/Icon;->createWithBitmap(Landroid/graphics/Bitmap;)Landroid/graphics/drawable/Icon;
    move-result-object v0
    return-object v0
.end method

.method private static getStateIcon(Ljava/lang/String;)Landroid/graphics/drawable/Icon;
    .locals 2

    if-nez p0, :chk_acting
    goto :icon_thinking

    :chk_acting
    const-string v0, "acting"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_workflow
    sget-object v0, LAgentKernel;->sIconActing:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x1
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconActing:Landroid/graphics/drawable/Icon;
    return-object v0

    :chk_workflow
    const-string v0, "workflow"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_confirm
    sget-object v0, LAgentKernel;->sIconWorkflow:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x2
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconWorkflow:Landroid/graphics/drawable/Icon;
    return-object v0

    :chk_confirm
    const-string v0, "confirm"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_done
    sget-object v0, LAgentKernel;->sIconConfirm:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x3
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconConfirm:Landroid/graphics/drawable/Icon;
    return-object v0

    :chk_done
    const-string v0, "done"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_stopped
    sget-object v0, LAgentKernel;->sIconDone:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x4
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconDone:Landroid/graphics/drawable/Icon;
    return-object v0

    :chk_stopped
    const-string v0, "stopped"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_error
    sget-object v0, LAgentKernel;->sIconStopped:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x5
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconStopped:Landroid/graphics/drawable/Icon;
    return-object v0

    :chk_error
    const-string v0, "error"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :icon_thinking
    sget-object v0, LAgentKernel;->sIconError:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x6
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconError:Landroid/graphics/drawable/Icon;
    return-object v0

    :icon_thinking
    sget-object v0, LAgentKernel;->sIconThinking:Landroid/graphics/drawable/Icon;
    if-nez v0, :ret_icon
    const/4 v1, 0x0
    invoke-static {v1}, LAgentKernel;->renderStateIcon(I)Landroid/graphics/drawable/Icon;
    move-result-object v0
    sput-object v0, LAgentKernel;->sIconThinking:Landroid/graphics/drawable/Icon;

    :ret_icon
    return-object v0
.end method

.method private static getStateColorHex(Ljava/lang/String;)Ljava/lang/String;
    .locals 1

    if-eqz p0, :def_hex
    const-string v0, "acting"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_wf_hex
    const-string v0, "#10B981"
    return-object v0

    :chk_wf_hex
    const-string v0, "workflow"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_cf_hex
    const-string v0, "#06B6D4"
    return-object v0

    :chk_cf_hex
    const-string v0, "confirm"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_dn_hex
    const-string v0, "#F59E0B"
    return-object v0

    :chk_dn_hex
    const-string v0, "done"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_st_hex
    const-string v0, "#22C55E"
    return-object v0

    :chk_st_hex
    const-string v0, "stopped"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-nez v0, :ret_rose_hex
    const-string v0, "error"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :def_hex

    :ret_rose_hex
    const-string v0, "#EF4444"
    return-object v0

    :def_hex
    const-string v0, "#6366F1"
    return-object v0
.end method

.method private static getStateColorInt(Ljava/lang/String;)I
    .locals 1

    if-eqz p0, :def_cint
    const-string v0, "acting"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_wf_cint
    const v0, -0xef467f
    return v0

    :chk_wf_cint
    const-string v0, "workflow"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_cf_cint
    const v0, -0xf9492c
    return v0

    :chk_cf_cint
    const-string v0, "confirm"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_dn_cint
    const v0, -0xa61f5
    return v0

    :chk_dn_cint
    const-string v0, "done"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :chk_st_cint
    const v0, -0xdd3aa2
    return v0

    :chk_st_cint
    const-string v0, "stopped"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-nez v0, :ret_rose_cint
    const-string v0, "error"
    invoke-virtual {v0, p0}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v0
    if-eqz v0, :def_cint

    :ret_rose_cint
    const v0, -0x10bbbc
    return v0

    :def_cint
    const v0, -0x9c990f
    return v0
.end method

.method private static buildFocusParamJson(Lorg/json/JSONObject;Ljava/lang/String;II)Ljava/lang/String;
    .locals 9
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Lorg/json/JSONException;
        }
    .end annotation

    const-string v0, "title"
    const-string v1, "Agent 执行中"
    invoke-virtual {p0, v0, v1}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v0

    const-string v1, "text"
    const-string v2, ""
    invoke-virtual {p0, v1, v2}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v1

    const-string v2, "short_text"
    invoke-virtual {p0, v2, v0}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v2

    const-string v3, "sub_text"
    const-string v4, "AI"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v3

    new-instance v4, Lorg/json/JSONObject;
    invoke-direct {v4}, Lorg/json/JSONObject;-><init>()V

    new-instance v5, Lorg/json/JSONObject;
    invoke-direct {v5}, Lorg/json/JSONObject;-><init>()V
    const-string v6, "business"
    const-string v7, "agent_live"
    invoke-virtual {v5, v6, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v6, "protocol"
    const/4 v7, 0x1
    invoke-virtual {v5, v6, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v6, "ticker"
    invoke-virtual {v5, v6, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v6, "tickerPic"
    const-string v8, "miui.focus.pic_ticker"
    invoke-virtual {v5, v6, v8}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v6, "enableFloat"
    const/4 v8, 0x0
    invoke-virtual {v5, v6, v8}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v6, "updatable"
    invoke-virtual {v5, v6, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v6, "isShowNotification"
    invoke-virtual {v5, v6, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v6, "timeout"
    invoke-virtual {v5, v6, p3}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;

    new-instance v6, Lorg/json/JSONObject;
    invoke-direct {v6}, Lorg/json/JSONObject;-><init>()V
    const-string v8, "picProfile"
    const-string p0, "miui.focus.pic_agent"
    invoke-virtual {v6, v8, p0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v8, "title"
    invoke-virtual {v6, v8, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v0, "content"
    invoke-virtual {v6, v0, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v0, "chatInfo"
    invoke-virtual {v5, v0, v6}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    if-ltz p2, :skip_focus_prog
    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v1, "progress"
    invoke-virtual {v0, v1, p2}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v1, "colorProgress"
    invoke-virtual {v0, v1, p1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v1, "colorProgressEnd"
    invoke-virtual {v0, v1, p1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v1, "isForward"
    invoke-virtual {v0, v1, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;Z)Lorg/json/JSONObject;
    const-string v1, "progressInfo"
    invoke-virtual {v5, v1, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    :skip_focus_prog
    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v1, "type"
    invoke-virtual {v0, v1, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v6, "pic"
    const-string v8, "miui.focus.pic_agent"
    invoke-virtual {v0, v6, v8}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v6, Lorg/json/JSONObject;
    invoke-direct {v6}, Lorg/json/JSONObject;-><init>()V
    const-string v8, "title"
    invoke-virtual {v6, v8, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v3, Lorg/json/JSONObject;
    invoke-direct {v3}, Lorg/json/JSONObject;-><init>()V
    invoke-virtual {v3, v1, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string p0, "picInfo"
    invoke-virtual {v3, p0, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string p1, "textInfo"
    invoke-virtual {v3, p1, v6}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v6, Lorg/json/JSONObject;
    invoke-direct {v6}, Lorg/json/JSONObject;-><init>()V
    invoke-virtual {v6, v8, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v2, Lorg/json/JSONObject;
    invoke-direct {v2}, Lorg/json/JSONObject;-><init>()V
    const/4 v8, 0x2
    invoke-virtual {v2, v1, v8}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    invoke-virtual {v2, p1, v6}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v1, Lorg/json/JSONObject;
    invoke-direct {v1}, Lorg/json/JSONObject;-><init>()V
    const-string v6, "imageTextInfoLeft"
    invoke-virtual {v1, v6, v3}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v3, "imageTextInfoRight"
    invoke-virtual {v1, v3, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v2, Lorg/json/JSONObject;
    invoke-direct {v2}, Lorg/json/JSONObject;-><init>()V
    invoke-virtual {v2, p0, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    new-instance v0, Lorg/json/JSONObject;
    invoke-direct {v0}, Lorg/json/JSONObject;-><init>()V
    const-string v3, "islandProperty"
    invoke-virtual {v0, v3, v7}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v3, "islandTimeout"
    invoke-virtual {v0, v3, p3}, Lorg/json/JSONObject;->put(Ljava/lang/String;I)Lorg/json/JSONObject;
    const-string v3, "bigIslandArea"
    invoke-virtual {v0, v3, v1}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v1, "smallIslandArea"
    invoke-virtual {v0, v1, v2}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    const-string v1, "param_island"
    invoke-virtual {v5, v1, v0}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;
    const-string v0, "param_v2"
    invoke-virtual {v4, v0, v5}, Lorg/json/JSONObject;->put(Ljava/lang/String;Ljava/lang/Object;)Lorg/json/JSONObject;

    invoke-virtual {v4}, Lorg/json/JSONObject;->toString()Ljava/lang/String;
    move-result-object v0
    return-object v0
.end method

.method private static handleNotifyProgress(Lorg/json/JSONObject;)Ljava/lang/String;
    .locals 14
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Throwable;
        }
    .end annotation

    invoke-static {}, LAgentKernel;->ensureNotifReady()V
    sget-object v0, LAgentKernel;->sNM:Landroid/app/INotificationManager;
    sget-object v1, LAgentKernel;->sCtx:Landroid/content/Context;

    const-string v3, "tag"
    const-string v4, "agent_live"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v7

    const-string v3, "id"
    const/16 v4, 0x26ac
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v8

    const-string v3, "state"
    const-string v4, "thinking"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v9

    const-string v3, "title"
    const-string v4, "Agent 执行中"
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v10

    const-string v3, "text"
    const-string v4, ""
    invoke-virtual {p0, v3, v4}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v11

    const-string v3, "short_text"
    invoke-virtual {p0, v3, v10}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v12

    invoke-static {v9}, LAgentKernel;->getStateIcon(Ljava/lang/String;)Landroid/graphics/drawable/Icon;
    move-result-object v2

    new-instance v3, Landroid/app/Notification$Builder;
    const-string v4, "agent_live_progress"
    invoke-direct {v3, v1, v4}, Landroid/app/Notification$Builder;-><init>(Landroid/content/Context;Ljava/lang/String;)V
    invoke-virtual {v3, v2}, Landroid/app/Notification$Builder;->setSmallIcon(Landroid/graphics/drawable/Icon;)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v2}, Landroid/app/Notification$Builder;->setLargeIcon(Landroid/graphics/drawable/Icon;)Landroid/app/Notification$Builder;
    invoke-static {v9}, LAgentKernel;->getStateColorInt(Ljava/lang/String;)I
    move-result v4
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setColor(I)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v10}, Landroid/app/Notification$Builder;->setContentTitle(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v11}, Landroid/app/Notification$Builder;->setContentText(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;

    const-string v4, "sub_text"
    const-string v5, ""
    invoke-virtual {p0, v4, v5}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v4
    invoke-virtual {v4}, Ljava/lang/String;->length()I
    move-result v5
    if-lez v5, :skip_sub
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setSubText(Ljava/lang/CharSequence;)Landroid/app/Notification$Builder;

    :skip_sub
    const/4 v4, 0x1
    const-string v5, "ongoing"
    invoke-virtual {p0, v5, v4}, Lorg/json/JSONObject;->optBoolean(Ljava/lang/String;Z)Z
    move-result v5
    invoke-virtual {v3, v5}, Landroid/app/Notification$Builder;->setOngoing(Z)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setOnlyAlertOnce(Z)Landroid/app/Notification$Builder;
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setShowWhen(Z)Landroid/app/Notification$Builder;

    const-string v4, "confirm"
    invoke-virtual {v4, v9}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v4
    if-eqz v4, :timeout_std
    const-wide v5, 0x493e0L
    const/16 v13, 0x12c
    goto :set_timeout

    :timeout_std
    const-wide v5, 0x2bf20L
    const/16 v13, 0xb4

    :set_timeout
    invoke-virtual {v3, v5, v6}, Landroid/app/Notification$Builder;->setTimeoutAfter(J)Landroid/app/Notification$Builder;

    :try_pi
    const-string v4, "click_uri"
    const-string v5, "http://127.0.0.1:8765/ui"
    invoke-virtual {p0, v4, v5}, Lorg/json/JSONObject;->optString(Ljava/lang/String;Ljava/lang/String;)Ljava/lang/String;
    move-result-object v4
    invoke-virtual {v4}, Ljava/lang/String;->trim()Ljava/lang/String;
    move-result-object v4
    invoke-virtual {v4}, Ljava/lang/String;->length()I
    move-result v5
    if-nez v5, :has_click_uri
    const-string v4, "http://127.0.0.1:8765/ui"

    :has_click_uri
    invoke-static {v4}, Landroid/net/Uri;->parse(Ljava/lang/String;)Landroid/net/Uri;
    move-result-object v4
    new-instance v5, Landroid/content/Intent;
    const-string v6, "android.intent.action.VIEW"
    invoke-direct {v5, v6, v4}, Landroid/content/Intent;-><init>(Ljava/lang/String;Landroid/net/Uri;)V
    const/high16 v4, 0x10000000
    invoke-virtual {v5, v4}, Landroid/content/Intent;->addFlags(I)Landroid/content/Intent;
    const/4 v4, 0x0
    const/high16 v6, 0x0c000000
    invoke-static {v1, v4, v5, v6}, Landroid/app/PendingIntent;->getActivity(Landroid/content/Context;ILandroid/content/Intent;I)Landroid/app/PendingIntent;
    move-result-object v4
    if-eqz v4, :pi_done_inner
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->setContentIntent(Landroid/app/PendingIntent;)Landroid/app/Notification$Builder;

    :pi_done_inner
    nop
    :try_end_pi
    .catch Ljava/lang/Throwable; {:try_pi .. :try_end_pi} :catch_pi
    goto :after_pi

    :catch_pi
    move-exception v4

    :after_pi
    const-string v4, "step"
    const/4 v5, 0x0
    invoke-virtual {p0, v4, v5}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v4
    const-string v6, "max"
    invoke-virtual {p0, v6, v5}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v5
    const-string v6, "progress"
    const/4 v1, -0x1
    invoke-virtual {p0, v6, v1}, Lorg/json/JSONObject;->optInt(Ljava/lang/String;I)I
    move-result v6

    if-lez v6, :chk_max_prog
    const/16 v1, 0x64
    if-le v6, v1, :prog_hi_ok
    move v6, v1
    :prog_hi_ok
    move v1, v6
    if-lez v5, :set_pct_bar
    const/4 v6, 0x0
    invoke-virtual {v3, v5, v4, v6}, Landroid/app/Notification$Builder;->setProgress(IIZ)Landroid/app/Notification$Builder;
    goto :prog_ready

    :set_pct_bar
    const/16 v4, 0x64
    const/4 v5, 0x0
    invoke-virtual {v3, v4, v1, v5}, Landroid/app/Notification$Builder;->setProgress(IIZ)Landroid/app/Notification$Builder;
    goto :prog_ready

    :chk_max_prog
    if-lez v5, :prog_ready
    if-gez v4, :chk_step_hi
    const/4 v4, 0x0
    :chk_step_hi
    if-le v4, v5, :calc_pct
    move v4, v5
    :calc_pct
    mul-int/lit8 v1, v4, 0x64
    div-int/2addr v1, v5
    const/4 v6, 0x0
    if-nez v4, :set_max_bar
    const-string v10, "starting"
    invoke-virtual {v10, v9}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v10
    if-eqz v10, :set_max_bar
    const/4 v6, 0x1

    :set_max_bar
    invoke-virtual {v3, v5, v4, v6}, Landroid/app/Notification$Builder;->setProgress(IIZ)Landroid/app/Notification$Builder;

    :prog_ready
    const-string v4, "focus"
    const/4 v5, 0x1
    invoke-virtual {p0, v4, v5}, Lorg/json/JSONObject;->optBoolean(Ljava/lang/String;Z)Z
    move-result v4
    if-eqz v4, :skip_focus

    new-instance v4, Landroid/os/Bundle;
    invoke-direct {v4}, Landroid/os/Bundle;-><init>()V
    const-string v6, "miui.focus.enable"
    invoke-virtual {v4, v6, v5}, Landroid/os/Bundle;->putBoolean(Ljava/lang/String;Z)V
    const-string v5, "miui.focus.ticker"
    invoke-virtual {v4, v5, v12}, Landroid/os/Bundle;->putString(Ljava/lang/String;Ljava/lang/String;)V

    new-instance v5, Landroid/os/Bundle;
    invoke-direct {v5}, Landroid/os/Bundle;-><init>()V
    const-string v6, "miui.focus.pic_agent"
    invoke-virtual {v5, v6, v2}, Landroid/os/Bundle;->putParcelable(Ljava/lang/String;Landroid/os/Parcelable;)V
    const-string v6, "miui.focus.pic_ticker"
    invoke-virtual {v5, v6, v2}, Landroid/os/Bundle;->putParcelable(Ljava/lang/String;Landroid/os/Parcelable;)V
    const-string v6, "miui.focus.pics"
    invoke-virtual {v4, v6, v5}, Landroid/os/Bundle;->putBundle(Ljava/lang/String;Landroid/os/Bundle;)V

    invoke-static {v9}, LAgentKernel;->getStateColorHex(Ljava/lang/String;)Ljava/lang/String;
    move-result-object v5
    invoke-static {p0, v5, v1, v13}, LAgentKernel;->buildFocusParamJson(Lorg/json/JSONObject;Ljava/lang/String;II)Ljava/lang/String;
    move-result-object v1
    const-string v5, "miui.focus.param"
    invoke-virtual {v4, v5, v1}, Landroid/os/Bundle;->putString(Ljava/lang/String;Ljava/lang/String;)V
    invoke-virtual {v3, v4}, Landroid/app/Notification$Builder;->addExtras(Landroid/os/Bundle;)Landroid/app/Notification$Builder;

    :skip_focus
    invoke-virtual {v3}, Landroid/app/Notification$Builder;->build()Landroid/app/Notification;
    move-result-object v5

    const-string v1, "com.android.shell"
    const-string v2, "com.android.shell"
    move-object v3, v7
    move v4, v8
    const/4 v6, 0x0
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
    invoke-static {}, Landroid/os/Looper;->myLooper()Landroid/os/Looper;
    move-result-object v0
    if-nez v0, :loop_looper_ok
    invoke-static {}, Landroid/os/Looper;->prepare()V
    :loop_looper_ok
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
