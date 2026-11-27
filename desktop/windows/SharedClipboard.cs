using System.Collections.Specialized;
using System.Drawing.Imaging;
using System.Runtime.InteropServices;
using System.Security.Cryptography;
using System.Text.Json;

namespace Cage.Windows;

internal static class SharedClipboard
{
    private static int generation;
    private static CancellationTokenSource sends = new();
    private static readonly Dictionary<string, string[]> copies = new();
    private static readonly List<string[]> readers = new();
    private static readonly HashSet<string> pending = new();
    private static readonly System.Windows.Forms.Timer timer = new() { Interval = 1000 };
    private static bool watching, loaded, collecting;
    private static string? lastImageDigest;

    // Hash dimensions and RGB pixels so an external clipboard manager (e.g.
    // remote desktop software) rewriting the same image as CF_DIB/PNG does not
    // trigger an endless clipboard ownership loop.
    private static string ComputeImageDigest(Image image)
    {
        var ownsBitmap = image is not Bitmap;
        var bitmap = image as Bitmap ?? new Bitmap(image);
        try
        {
            var rect = new Rectangle(0, 0, bitmap.Width, bitmap.Height);
            var data = bitmap.LockBits(rect, ImageLockMode.ReadOnly, PixelFormat.Format24bppRgb);
            try
            {
                using var hash = IncrementalHash.CreateHash(HashAlgorithmName.SHA256);
                Span<byte> dims = stackalloc byte[8];
                BitConverter.TryWriteBytes(dims[..4], bitmap.Width);
                BitConverter.TryWriteBytes(dims[4..], bitmap.Height);
                hash.AppendData(dims);
                var rowBytes = bitmap.Width * 3;
                var row = new byte[rowBytes];
                for (var y = 0; y < bitmap.Height; y++)
                {
                    Marshal.Copy(IntPtr.Add(data.Scan0, y * data.Stride), row, 0, rowBytes);
                    hash.AppendData(row);
                }
                return Convert.ToHexString(hash.GetHashAndReset());
            }
            finally { bitmap.UnlockBits(data); }
        }
        finally
        {
            if (ownsBitmap) bitmap.Dispose();
        }
    }

    public static void Cancel()
    {
        generation++;
        var canceled = sends; sends = new();
        canceled.Cancel(); canceled.Dispose();
    }
    [DllImport("user32.dll")]
    private static extern uint GetClipboardSequenceNumber();

    // Synchronization reads the clipboard through Win32 instead of the WinForms
    // Clipboard class. Each WinForms call opens the OLE clipboard again and,
    // when another application holds it, sleeps 100 ms on the UI thread up to
    // ten times; a read that still fails comes back as empty text. Clipboard
    // managers, input methods and chat apps open the clipboard after every
    // copy, so polling met that often: the window froze, and the guest
    // received an empty clipboard that the next paste inserted as nothing.
    // Format checks below do not open the clipboard. The data read opens it
    // once, retries without blocking the window, and reports a held clipboard
    // as busy so the workbench reads it again instead of guessing.
    [DllImport("user32.dll", SetLastError = true)] private static extern bool OpenClipboard(IntPtr owner);
    [DllImport("user32.dll", SetLastError = true)] private static extern bool CloseClipboard();
    [DllImport("user32.dll")] private static extern bool IsClipboardFormatAvailable(uint format);
    [DllImport("user32.dll")] private static extern IntPtr GetClipboardData(uint format);
    [DllImport("user32.dll", EntryPoint = "RegisterClipboardFormatW", CharSet = CharSet.Unicode)] private static extern uint RegisterClipboardFormat(string name);
    [DllImport("kernel32.dll")] private static extern IntPtr GlobalLock(IntPtr memory);
    [DllImport("kernel32.dll")] private static extern bool GlobalUnlock(IntPtr memory);
    [DllImport("kernel32.dll")] private static extern UIntPtr GlobalSize(IntPtr memory);
    [DllImport("shell32.dll", EntryPoint = "DragQueryFileW", CharSet = CharSet.Unicode)] private static extern uint DragQueryFile(IntPtr drop, uint index, [Out] char[]? path, uint length);
    private const uint BitmapFormat = 2, DibFormat = 8, UnicodeTextFormat = 13, FileDropFormat = 15, DibV5Format = 17;
    private static readonly uint RichTextFormat = RegisterClipboardFormat(DataFormats.Rtf);
    // The spreadsheet formats RichText() treats as a cell range.
    private static readonly uint[] CellFormats =
        new[] { "Biff12", "Biff8", "Biff5", "XML Spreadsheet", DataFormats.CommaSeparatedValue }
            .Select(RegisterClipboardFormat).Where(format => format != 0).ToArray();

    private sealed class ClipboardBusyException : Exception { }

    private static bool HasImage() =>
        IsClipboardFormatAvailable(BitmapFormat) || IsClipboardFormatAvailable(DibFormat) || IsClipboardFormatAvailable(DibV5Format);

    // Never hold the clipboard across an await: other applications wait for it.
    private static async Task<T> ReadOpen<T>(Func<T?> read, CancellationToken token) where T : class
    {
        for (var attempt = 0; ; attempt++)
        {
            if (OpenClipboard(IntPtr.Zero))
            {
                T? value;
                try { value = read(); }
                finally { CloseClipboard(); }
                // A listed format without data was replaced or failed to render.
                return value ?? throw new ClipboardBusyException();
            }
            if (attempt == 6) throw new ClipboardBusyException();
            await Task.Delay(attempt < 3 ? 5 : 20, token);
        }
    }

    private static string? OpenText()
    {
        var memory = GetClipboardData(UnicodeTextFormat);
        if (memory == IntPtr.Zero) return null;
        var pointer = GlobalLock(memory);
        if (pointer == IntPtr.Zero) return null;
        try
        {
            var text = Marshal.PtrToStringUni(pointer, (int)Math.Min((ulong)GlobalSize(memory) / 2, int.MaxValue));
            var end = text.IndexOf('\0');
            return end < 0 ? text : text[..end];
        }
        finally { GlobalUnlock(memory); }
    }

    private static string[]? OpenFiles()
    {
        var drop = GetClipboardData(FileDropFormat);
        if (drop == IntPtr.Zero) return null;
        var paths = new string[DragQueryFile(drop, uint.MaxValue, null, 0)];
        for (uint index = 0; index < paths.Length; index++)
        {
            var length = DragQueryFile(drop, index, null, 0);
            var path = new char[length + 1];
            if (DragQueryFile(drop, index, path, length + 1) != length) return null;
            paths[index] = new string(path, 0, (int)length);
        }
        return paths;
    }

    // The same conversion WinForms applies to a copied picture. The clipboard
    // keeps its bitmap; FromHbitmap copies the pixels.
    private static Bitmap? OpenImage()
    {
        var bitmap = GetClipboardData(BitmapFormat);
        return bitmap == IntPtr.Zero ? null : Image.FromHbitmap(bitmap);
    }

    private static async Task<string[]> SelectedFiles(CancellationToken token) =>
        IsClipboardFormatAvailable(FileDropFormat) ? await ReadOpen(OpenFiles, token) : [];
    public static uint Revision
    {
        get
        {
            if (!watching)
            {
                watching = true;
                timer.Tick += async (_, _) => await Collect();
                timer.Start();
            }
            return GetClipboardSequenceNumber();
        }
    }

    private static async Task<JsonElement> Cache(string action, string id = "")
    {
        var request = JsonSerializer.Serialize(new { action, cache_id = id });
        var result = await Backend.Json(["clipboard-files", "--json", "cache", request], CancellationToken.None, TimeSpan.FromSeconds(10), outputLimit: 30 << 20);
        if (result.Exit != 0) throw new IOException("clipboard_cache_operation_failed");
        return result.Data;
    }

    private static bool References(string[] selected, string[] owned)
    {
        var root = Path.GetDirectoryName(owned[0])!;
        return selected.Any(path => Path.GetFullPath(path).Equals(root, StringComparison.OrdinalIgnoreCase)
            || Path.GetFullPath(path).StartsWith(root + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase)
            || root.StartsWith(Path.GetFullPath(path).TrimEnd(Path.DirectorySeparatorChar) + Path.DirectorySeparatorChar, StringComparison.OrdinalIgnoreCase));
    }

    private static async Task ReleaseLocal(string id)
    {
        try { await Cache("release", id); copies.Remove(id); }
        catch { /* The receipt remains available for the next ownership check. */ }
    }

    private static async Task Collect()
    {
        if (collecting) return;
        collecting = true;
        try
        {
            if (!loaded)
            {
                var result = await Cache("list");
                foreach (var item in result.GetProperty("selections").EnumerateArray())
                    copies.TryAdd(item.Text("cache_id"), item.GetProperty("paths").EnumerateArray().Select(path => path.GetString()!).ToArray());
                loaded = true;
            }
            if (copies.Count == 0) return;
            // Read the selection once per check; a busy clipboard retains its files.
            var selected = await SelectedFiles(CancellationToken.None);
            foreach (var copy in copies.ToArray())
            {
                if (pending.Contains(copy.Key) || References(selected, copy.Value) || readers.Any(paths => References(paths, copy.Value))) continue;
                await ReleaseLocal(copy.Key);
            }
        }
        catch { /* An unreadable OS clipboard retains its files. */ }
        finally { collecting = false; }
    }

    private static string ConnectionID(JsonElement request)
    {
        var connection = request.Text("connection_id");
        return string.IsNullOrEmpty(connection) ? "local" : connection;
    }

    private static async Task ReleaseRemote(string connection, string id, CancellationToken token)
    {
        var response = await Backend.Json(["clipboard-files", "--json", "--connection", string.IsNullOrEmpty(connection) ? "local" : connection, "release", id], token, TimeSpan.FromSeconds(10));
        if (response.Exit != 0) throw new IOException("clipboard_selection_release_failed");
    }

    public static async Task<object> Release(JsonElement request, CancellationToken token)
    {
        var id = request.Text("files");
        if (!Guid.TryParse(id, out _)) throw new InvalidDataException("剪贴板文件标识无效");
        await ReleaseRemote(ConnectionID(request), id, token);
        return new { ok = true };
    }

    // A cell range can advertise a bitmap preview without advertising RTF.
    // Even an empty formatted cell should keep its native formats on the host.
    public static string? RichText(IDataObject? data)
    {
        if (data == null) return null;
        var formats = data.GetFormats(autoConvert: false);
        var cells = formats.Any(format =>
            format is "Biff12" or "Biff8" or "Biff5" or "XML Spreadsheet" || format == DataFormats.CommaSeparatedValue);
        if (!cells && !formats.Contains(DataFormats.Rtf)) return null;
        if (data.TryGetData<string>(DataFormats.UnicodeText, autoConvert: true, out var text) && text.Length > 0) return text;
        return cells ? "" : null;
    }

    public static async Task<object> Read(JsonElement request, CancellationToken token)
    {
        var revision = Revision;
        try { return await ReadSnapshot(request, revision, token); }
        catch (ClipboardBusyException) { return new { revision, busy = true }; }
    }

    private static async Task<object> ReadSnapshot(JsonElement request, uint revision, CancellationToken token)
    {
        var capturedGeneration = generation;
        if (IsClipboardFormatAvailable(FileDropFormat))
        {
            var selected = await ReadOpen(OpenFiles, token);
            lastImageDigest = null;
            if (request.TryGetProperty("defer_files", out var defer) && defer.ValueKind == JsonValueKind.True)
                return new { revision, files_pending = true, count = selected.Length };
            using var cancellation = CancellationTokenSource.CreateLinkedTokenSource(token, sends.Token);
            readers.Add(selected);
            try
            {
                var connection = ConnectionID(request);
                var arguments = new List<string> { "clipboard-files", "--json", "--connection", connection, "send" };
                arguments.AddRange(selected);
                var response = await Backend.Json(arguments.ToArray(), cancellation.Token, TimeSpan.FromMinutes(30));
                if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                var files = response.Data.Text("files");
                if (capturedGeneration != generation || revision != Revision)
                {
                    try { await ReleaseRemote(connection, files, CancellationToken.None); } catch { }
                }
                return new { revision, files };
            }
            catch (OperationCanceledException) { throw new IOException("剪贴板文件传递已取消"); }
            finally { readers.Remove(selected); await Collect(); }
        }
        // Same rule as RichText(): a cell range, or rich text with a rendered
        // picture, is text. An empty formatted cell stays text too.
        var cells = CellFormats.Any(IsClipboardFormatAvailable);
        if (cells || IsClipboardFormatAvailable(RichTextFormat))
        {
            var copied = IsClipboardFormatAvailable(UnicodeTextFormat) ? await ReadOpen(OpenText, token) : "";
            if (copied.Length > 0 || cells)
            {
                lastImageDigest = null;
                return new { revision, text = copied };
            }
        }
        if (HasImage())
        {
            using var image = await ReadOpen(OpenImage, token);
            if ((long)image.Width * image.Height > 40_000_000) throw new IOException("图片不能超过 4000 万像素");
            var initial = request.TryGetProperty("initial", out var initProp) && initProp.ValueKind == JsonValueKind.True;
            var previous = lastImageDigest;
            // Hashing and PNG-encoding a screenshot takes long enough to freeze
            // the window, and the clipboard is already closed: encode elsewhere.
            var (digest, png) = await Task.Run(() =>
            {
                var hash = ComputeImageDigest(image);
                if (!initial && hash == previous) return (hash, (byte[]?)null);
                using var output = new MemoryStream();
                image.Save(output, System.Drawing.Imaging.ImageFormat.Png);
                return (hash, output.Length > 20 * 1024 * 1024 ? Array.Empty<byte>() : output.ToArray());
            }, token);
            if (png == null) return new { revision, unchanged = true };
            if (png.Length == 0) throw new IOException("图片不能超过 20 MB");
            lastImageDigest = digest;
            return new { revision, png = Convert.ToBase64String(png) };
        }
        lastImageDigest = null;
        // An empty clipboard and one holding only other formats share as
        // empty text. A text copy that cannot be read now is busy, not empty.
        return new { revision, text = IsClipboardFormatAvailable(UnicodeTextFormat) ? await ReadOpen(OpenText, token) : "" };
    }

    public static async Task<object> Write(JsonElement request, CancellationToken token)
    {
        _ = Revision;
        var currentGeneration = generation;
        string[]? paths = null;
        var cache = "";
        if (request.Text("files") is { Length: > 0 } id)
        {
            if (!Guid.TryParse(id, out _)) throw new InvalidDataException("剪贴板文件标识无效");
            try
            {
                var response = await Backend.Json(["clipboard-files", "--json", "--connection", ConnectionID(request), "receive", id], token, TimeSpan.FromMinutes(30));
                if (response.Exit != 0) throw new IOException(response.Data.Text("message"));
                paths = response.Data.GetProperty("paths").EnumerateArray().Select(value => value.GetString()!).ToArray();
                cache = response.Data.Text("cache_id");
                if (cache.Length != 0) { copies[cache] = paths; pending.Add(cache); }
            }
            catch { loaded = false; throw; }
        }
        try
        {
            if (currentGeneration != generation || request.TryGetProperty("revision", out var expected) && expected.GetUInt32() != Revision)
            {
                if (cache.Length != 0) await ReleaseLocal(cache);
                return new { revision = Revision, superseded = true };
            }
            if (paths != null)
            {
                lastImageDigest = null;
                var files = new StringCollection(); files.AddRange(paths); Clipboard.SetFileDropList(files);
            }
            else if (request.Text("png") is { Length: > 0 } png)
            {
                var bytes = Convert.FromBase64String(png);
                if (bytes.Length > 20 * 1024 * 1024) throw new InvalidDataException("图片不能超过 20 MB");
                using var input = new MemoryStream(bytes);
                using var image = Image.FromStream(input);
                if ((long)image.Width * image.Height > 40_000_000) throw new InvalidDataException("图片不能超过 4000 万像素");
                var digest = ComputeImageDigest(image);
                var sameOnClipboard = false;
                if (Clipboard.ContainsImage())
                {
                    try
                    {
                        using var existing = Clipboard.GetImage();
                        if (existing != null && (long)existing.Width * existing.Height <= 40_000_000)
                        {
                            sameOnClipboard = ComputeImageDigest(existing) == digest;
                        }
                    }
                    catch { /* Fall through to write if existing image cannot be inspected. */ }
                }
                lastImageDigest = digest;
                if (!sameOnClipboard) Clipboard.SetImage(image);
            }
            else if (request.TryGetProperty("text", out var text))
            {
                lastImageDigest = null;
                var value = text.GetString() ?? "";
                if (System.Text.Encoding.UTF8.GetByteCount(value) > 2 * 1024 * 1024) throw new InvalidDataException("文本不能超过 2 MB");
                // A guest may echo the cell text with Unix line endings. Keep
                // the original clipboard owner, formulas and formatting.
                var existing = RichText(Clipboard.GetDataObject());
                if (existing == null || existing.Replace("\r\n", "\n") != value.Replace("\r\n", "\n"))
                {
                    if (value.Length == 0) Clipboard.Clear(); else Clipboard.SetText(value);
                }
            }
            else throw new InvalidDataException("剪贴板格式无效");
            var writtenRevision = Revision;
            if (cache.Length != 0) { try { await Cache("publish", cache); } catch { } }
            return new { revision = writtenRevision };
        }
        finally { pending.Remove(cache); await Collect(); }
    }
}
