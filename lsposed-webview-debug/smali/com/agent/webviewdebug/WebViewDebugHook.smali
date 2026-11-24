.class public Lcom/agent/webviewdebug/WebViewDebugHook;
.super Ljava/lang/Object;
.implements Lde/robv/android/xposed/IXposedHookLoadPackage;

.field static volatile sEnabled:Z

.method public constructor <init>()V
    .locals 0
    invoke-direct {p0}, Ljava/lang/Object;-><init>()V
    return-void
.end method

.method public handleLoadPackage(Lde/robv/android/xposed/callbacks/XC_LoadPackage$LoadPackageParam;)V
    .locals 5
    .param p1, "lpparam"    # Lde/robv/android/xposed/callbacks/XC_LoadPackage$LoadPackageParam;

    :try_start_all
    const-class v0, Landroid/webkit/WebView;

    # 1. Hook WebView.setWebContentsDebuggingEnabled(boolean) to always force true
    const-string v1, "setWebContentsDebuggingEnabled"
    const/4 v2, 0x2
    new-array v2, v2, [Ljava/lang/Object;
    const/4 v3, 0x0
    sget-object v4, Ljava/lang/Boolean;->TYPE:Ljava/lang/Class;
    aput-object v4, v2, v3
    const/4 v3, 0x1
    new-instance v4, Lcom/agent/webviewdebug/ForceTrueCallback;
    invoke-direct {v4}, Lcom/agent/webviewdebug/ForceTrueCallback;-><init>()V
    aput-object v4, v2, v3
    invoke-static {v0, v1, v2}, Lde/robv/android/xposed/XposedHelpers;->findAndHookMethod(Ljava/lang/Class;Ljava/lang/String;[Ljava/lang/Object;)Lde/robv/android/xposed/XC_MethodHook$Unhook;

    # 2. Hook all WebView constructors to enable debugging once on first WebView init (UI thread safe)
    new-instance v1, Lcom/agent/webviewdebug/WebViewInitCallback;
    invoke-direct {v1}, Lcom/agent/webviewdebug/WebViewInitCallback;-><init>()V
    invoke-static {v0, v1}, Lde/robv/android/xposed/XposedBridge;->hookAllConstructors(Ljava/lang/Class;Lde/robv/android/xposed/XC_MethodHook;)Ljava/util/Set;

    const-string v1, "[AgentWebViewDebug] hooks installed"
    invoke-static {v1}, Lde/robv/android/xposed/XposedBridge;->log(Ljava/lang/String;)V
    :try_end_all
    .catch Ljava/lang/Throwable; {:try_start_all .. :try_end_all} :catch_all
    return-void

    :catch_all
    move-exception v0
    invoke-static {v0}, Lde/robv/android/xposed/XposedBridge;->log(Ljava/lang/Throwable;)V
    return-void
.end method
