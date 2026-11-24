.class public Lcom/agent/webviewdebug/WebViewInitCallback;
.super Lde/robv/android/xposed/XC_MethodHook;

.method public constructor <init>()V
    .locals 0
    invoke-direct {p0}, Lde/robv/android/xposed/XC_MethodHook;-><init>()V
    return-void
.end method

.method protected beforeHookedMethod(Lde/robv/android/xposed/XC_MethodHook$MethodHookParam;)V
    .locals 1
    .param p1, "param"    # Lde/robv/android/xposed/XC_MethodHook$MethodHookParam;

    :try_start
    sget-boolean v0, Lcom/agent/webviewdebug/WebViewDebugHook;->sEnabled:Z
    if-nez v0, :done
    const/4 v0, 0x1
    sput-boolean v0, Lcom/agent/webviewdebug/WebViewDebugHook;->sEnabled:Z
    invoke-static {v0}, Landroid/webkit/WebView;->setWebContentsDebuggingEnabled(Z)V
    :try_end
    .catch Ljava/lang/Throwable; {:try_start .. :try_end} :catch_err
    :done
    return-void

    :catch_err
    move-exception v0
    return-void
.end method
