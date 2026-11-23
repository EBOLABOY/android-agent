.class public LAgentClip;
.super Ljava/lang/Object;

.method public constructor <init>()V
    .locals 0
    invoke-direct {p0}, Ljava/lang/Object;-><init>()V
    return-void
.end method

.method private static readAllUtf8(Ljava/io/InputStream;)Ljava/lang/String;
    .locals 4
    .param p0, "in"    # Ljava/io/InputStream;
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/io/IOException;
        }
    .end annotation

    new-instance v0, Ljava/io/ByteArrayOutputStream;
    invoke-direct {v0}, Ljava/io/ByteArrayOutputStream;-><init>()V

    const/16 v1, 0x1000
    new-array v1, v1, [B

    :loop
    invoke-virtual {p0, v1}, Ljava/io/InputStream;->read([B)I
    move-result v2
    const/4 v3, -0x1
    if-eq v2, v3, :done
    const/4 v3, 0x0
    invoke-virtual {v0, v1, v3, v2}, Ljava/io/ByteArrayOutputStream;->write([BII)V
    goto :loop

    :done
    const-string v2, "UTF-8"
    invoke-virtual {v0, v2}, Ljava/io/ByteArrayOutputStream;->toString(Ljava/lang/String;)Ljava/lang/String;
    move-result-object v2
    return-object v2
.end method

.method public static main([Ljava/lang/String;)V
    .locals 9
    .param p0, "args"    # [Ljava/lang/String;
    .annotation system Ldalvik/annotation/Throws;
        value = {
            Ljava/lang/Exception;
        }
    .end annotation

    array-length v0, p0
    if-nez v0, :has_args
    sget-object v0, Ljava/lang/System;->err:Ljava/io/PrintStream;
    const-string v1, "usage: AgentClip <set|get>"
    invoke-virtual {v0, v1}, Ljava/io/PrintStream;->println(Ljava/lang/String;)V
    const/4 v0, 0x2
    invoke-static {v0}, Ljava/lang/System;->exit(I)V
    return-void

    :has_args
    const-string v0, "clipboard"
    invoke-static {v0}, Landroid/os/ServiceManager;->getService(Ljava/lang/String;)Landroid/os/IBinder;
    move-result-object v0
    if-nez v0, :got_binder
    new-instance v1, Ljava/lang/RuntimeException;
    const-string v2, "clipboard service unavailable"
    invoke-direct {v1, v2}, Ljava/lang/RuntimeException;-><init>(Ljava/lang/String;)V
    throw v1

    :got_binder
    invoke-static {v0}, Landroid/content/IClipboard$Stub;->asInterface(Landroid/os/IBinder;)Landroid/content/IClipboard;
    move-result-object v0

    const/4 v6, 0x0
    aget-object v6, p0, v6

    const-string v7, "set"
    invoke-virtual {v7, v6}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v7
    if-eqz v7, :check_get

    sget-object v7, Ljava/lang/System;->in:Ljava/io/InputStream;
    invoke-static {v7}, LAgentClip;->readAllUtf8(Ljava/io/InputStream;)Ljava/lang/String;
    move-result-object v7

    const-string v8, "agent"
    invoke-static {v8, v7}, Landroid/content/ClipData;->newPlainText(Ljava/lang/CharSequence;Ljava/lang/CharSequence;)Landroid/content/ClipData;
    move-result-object v1

    const-string v2, "com.android.shell"
    const/4 v3, 0x0
    const/4 v4, 0x0
    const/4 v5, 0x0
    invoke-interface/range {v0 .. v5}, Landroid/content/IClipboard;->setPrimaryClip(Landroid/content/ClipData;Ljava/lang/String;Ljava/lang/String;II)V

    sget-object v7, Ljava/lang/System;->out:Ljava/io/PrintStream;
    const-string v8, "OK"
    invoke-virtual {v7, v8}, Ljava/io/PrintStream;->println(Ljava/lang/String;)V
    return-void

    :check_get
    const-string v7, "get"
    invoke-virtual {v7, v6}, Ljava/lang/String;->equals(Ljava/lang/Object;)Z
    move-result v7
    if-eqz v7, :bad_cmd

    const-string v1, "com.android.shell"
    const/4 v2, 0x0
    const/4 v3, 0x0
    const/4 v4, 0x0
    invoke-interface {v0, v1, v2, v3, v4}, Landroid/content/IClipboard;->getPrimaryClip(Ljava/lang/String;Ljava/lang/String;II)Landroid/content/ClipData;
    move-result-object v5
    if-eqz v5, :empty_get

    invoke-virtual {v5}, Landroid/content/ClipData;->getItemCount()I
    move-result v6
    if-lez v6, :empty_get

    const/4 v6, 0x0
    invoke-virtual {v5, v6}, Landroid/content/ClipData;->getItemAt(I)Landroid/content/ClipData$Item;
    move-result-object v6
    if-eqz v6, :empty_get

    invoke-virtual {v6}, Landroid/content/ClipData$Item;->getText()Ljava/lang/CharSequence;
    move-result-object v7
    if-eqz v7, :empty_get

    invoke-interface {v7}, Ljava/lang/CharSequence;->toString()Ljava/lang/String;
    move-result-object v7
    const-string v8, "UTF-8"
    invoke-virtual {v7, v8}, Ljava/lang/String;->getBytes(Ljava/lang/String;)[B
    move-result-object v7
    sget-object v8, Ljava/lang/System;->out:Ljava/io/PrintStream;
    invoke-virtual {v8, v7}, Ljava/io/PrintStream;->write([B)V
    invoke-virtual {v8}, Ljava/io/PrintStream;->flush()V

    :empty_get
    return-void

    :bad_cmd
    sget-object v7, Ljava/lang/System;->err:Ljava/io/PrintStream;
    const-string v8, "unknown command"
    invoke-virtual {v7, v8}, Ljava/io/PrintStream;->println(Ljava/lang/String;)V
    const/4 v7, 0x2
    invoke-static {v7}, Ljava/lang/System;->exit(I)V
    return-void
.end method
