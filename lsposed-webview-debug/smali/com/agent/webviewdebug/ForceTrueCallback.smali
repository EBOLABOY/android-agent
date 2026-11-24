.class public Lcom/agent/webviewdebug/ForceTrueCallback;
.super Lde/robv/android/xposed/XC_MethodHook;

.method public constructor <init>()V
    .locals 0
    invoke-direct {p0}, Lde/robv/android/xposed/XC_MethodHook;-><init>()V
    return-void
.end method

.method protected beforeHookedMethod(Lde/robv/android/xposed/XC_MethodHook$MethodHookParam;)V
    .locals 3
    .param p1, "param"    # Lde/robv/android/xposed/XC_MethodHook$MethodHookParam;

    :try_start
    iget-object v0, p1, Lde/robv/android/xposed/XC_MethodHook$MethodHookParam;->args:[Ljava/lang/Object;
    if-eqz v0, :done
    array-length v1, v0
    if-lez v1, :done
    const/4 v1, 0x0
    sget-object v2, Ljava/lang/Boolean;->TRUE:Ljava/lang/Boolean;
    aput-object v2, v0, v1
    :try_end
    .catch Ljava/lang/Throwable; {:try_start .. :try_end} :catch_err
    :done
    return-void

    :catch_err
    move-exception v0
    return-void
.end method
