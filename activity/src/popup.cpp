#include "popup.h"

#include "backend.h"

#include <QGuiApplication>
#include <QQuickWindow>
#include <QScreen>

#ifdef HAVE_LAYERSHELLQT
#include <LayerShellQt/Window>
#endif

static constexpr int kMargin = 8;

Popup::Popup(QQuickWindow *window, Backend *backend, QObject *parent)
    : QObject(parent), m_window(window), m_backend(backend)
{
    m_window->setFlags(Qt::FramelessWindowHint | Qt::Tool | Qt::WindowStaysOnTopHint);

#ifdef HAVE_LAYERSHELLQT
    if (QGuiApplication::platformName().startsWith(QLatin1String("wayland"))) {
        // must be configured before the window is first shown
        if (auto *lw = LayerShellQt::Window::get(m_window)) {
            lw->setScope(QStringLiteral("onedriver-activity"));
            lw->setLayer(LayerShellQt::Window::LayerTop);
            lw->setAnchors(LayerShellQt::Window::Anchors(LayerShellQt::Window::AnchorBottom | LayerShellQt::Window::AnchorRight));
            lw->setMargins(QMargins(0, 0, kMargin, kMargin));
            lw->setExclusiveZone(0);
            lw->setKeyboardInteractivity(LayerShellQt::Window::KeyboardInteractivityOnDemand);
            m_layerShell = true;
        }
    }
#endif

    connect(m_window, &QWindow::activeChanged, this, &Popup::onActiveChanged);
    connect(m_window, &QWindow::visibleChanged, this, [this](bool visible) {
        m_backend->setVisible(visible);
        if (visible) {
            m_wasActive = false;
        }
    });
}

void Popup::Toggle()
{
    if (m_window->isVisible()) {
        m_window->hide();
        return;
    }
    // Clicking the tray icon first takes the focus away from the open window
    // (which hides it) and then calls Toggle. Don't reopen it right away.
    if (m_hiddenByFocusLoss.isValid() && m_hiddenByFocusLoss.elapsed() < 400) {
        return;
    }
    show();
}

void Popup::show()
{
    if (!m_layerShell) {
        position();
    }
    m_window->show();
    m_window->requestActivate();
}

void Popup::position()
{
    QScreen *screen = QGuiApplication::primaryScreen();
    if (!screen) {
        return;
    }
    const QRect area = screen->availableGeometry();
    m_window->setPosition(area.right() - m_window->width() - kMargin + 1, area.bottom() - m_window->height() - kMargin + 1);
}

void Popup::onActiveChanged()
{
    if (m_window->isActive()) {
        m_wasActive = true;
    } else if (m_wasActive && m_window->isVisible()) {
        m_hiddenByFocusLoss.restart();
        m_window->hide();
    }
}
