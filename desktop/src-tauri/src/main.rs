// LongCat-frontend Desktop — Tauri v2 壳。
//
// 启动时拉起 Go 后端（LongCat-frontend serve），窗口加载其 Web UI；
// 退出时结束后端进程。保持整体轻量：Rust 侧只做进程管理 + WebView。
#![cfg_attr(not(debug_assertions), windows_subsystem = "windows")]

use std::net::TcpStream;
use std::path::PathBuf;
use std::process::{Child, Command};
use std::sync::Mutex;
use std::time::{Duration, Instant};

use tauri::Manager;

const BACKEND_ADDR: &str = "127.0.0.1:5510";

struct Backend(Mutex<Option<Child>>);

#[tauri::command]
async fn pick_folder() -> Option<String> {
    rfd::AsyncFileDialog::new()
        .set_title("打开工作文件夹")
        .pick_folder()
        .await
        .map(|handle| handle.path().to_string_lossy().to_string())
}

#[tauri::command]
fn reveal_folder(path: String) -> Result<(), String> {
    let folder = PathBuf::from(path);
    if !folder.is_dir() { return Err("文件夹不存在或不可用".into()); }
    #[cfg(target_os = "windows")]
    {
        Command::new("explorer.exe").arg(&folder).spawn().map_err(|e| e.to_string())?;
    }
    #[cfg(target_os = "macos")]
    {
        Command::new("open").arg(&folder).spawn().map_err(|e| e.to_string())?;
    }
    #[cfg(all(unix, not(target_os = "macos")))]
    {
        Command::new("xdg-open").arg(&folder).spawn().map_err(|e| e.to_string())?;
    }
    Ok(())
}

/// 依次在常见位置查找 Go 后端可执行文件。
fn find_backend() -> Option<PathBuf> {
    let exe_name = if cfg!(windows) { "LongCat-frontend.exe" } else { "LongCat-frontend" };
    let mut candidates: Vec<PathBuf> = Vec::new();
    if let Ok(exe) = std::env::current_exe() {
        if let Some(dir) = exe.parent() {
            // 安装包布局: bundle.resources 的 "LongCat-frontend.exe" 落到 INSTALLDIR（与桌面壳同级）
            candidates.push(dir.join(exe_name));
            // 开发布局: desktop/src-tauri/target/{debug,release}/ -> 项目根 bin/
            candidates.push(dir.join("../../../../bin").join(exe_name));
            // 开发布局(备选): -> desktop/bin/
            candidates.push(dir.join("../../../bin").join(exe_name));
        }
    }
    candidates.push(PathBuf::from("bin").join(exe_name));
    candidates.into_iter().find(|p| p.exists())
}

fn wait_ready(addr: &str, timeout: Duration) -> bool {
    let start = Instant::now();
    while start.elapsed() < timeout {
        if TcpStream::connect(addr).is_ok() {
            return true;
        }
        std::thread::sleep(Duration::from_millis(120));
    }
    false
}

fn main() {
    // 已有后端在运行则直接复用，否则拉起。
    let child = if TcpStream::connect(BACKEND_ADDR).is_ok() {
        None
    } else {
        let path = find_backend().expect(
            "找不到 LongCat-frontend 可执行文件，请先运行: go build -o bin/LongCat-frontend.exe ./cmd/LongCat-frontend",
        );
        let child = Command::new(path)
            .args(["serve", "-addr", BACKEND_ADDR])
            .spawn()
            .expect("启动 Go 后端失败");
        if !wait_ready(BACKEND_ADDR, Duration::from_secs(10)) {
            panic!("Go 后端在 10s 内未就绪: {BACKEND_ADDR}");
        }
        Some(child)
    };

    tauri::Builder::default()
        .manage(Backend(Mutex::new(child)))
        .invoke_handler(tauri::generate_handler![pick_folder, reveal_folder])
        .on_window_event(|window, event| {
            if let tauri::WindowEvent::Destroyed = event {
                let state: tauri::State<Backend> = window.state();
                let child: Option<Child> = state.0.lock().unwrap().take();
                if let Some(mut c) = child {
                    let _ = c.kill();
                }
            }
        })
        .run(tauri::generate_context!())
        .expect("Tauri 运行失败");
}
