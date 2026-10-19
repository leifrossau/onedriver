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
    // the layer must be set up before the window is first shown
    m_layerShell = QGuiApplication::platformName().startsWith(QLatin1String("wayland")) && LayerShellQt::Window::get(m_window);
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
    toggle(std::nullopt);
}

void Popup::ToggleAt(int x, int y)
{
    toggle(QPoint(x, y));
}

void Popup::toggle(std::optional<QPoint> anchor)
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
    m_anchor = anchor;
    show();
}

void Popup::show()
{
    position();
    m_window->show();
    m_window->requestActivate();
}

// Puts the window on the screen of the anchor (the primary screen without
// one), above or below the anchor depending on which half of the screen it is
// in, and centered on it horizontally as far as the screen allows. Without an
// anchor it goes to the bottom right corner.
void Popup::position()
{
    QScreen *screen = m_anchor ? QGuiApplication::screenAt(*m_anchor) : nullptr;
    if (!screen) {
        screen = QGuiApplication::primaryScreen();
    }
    if (!screen) {
        return;
    }
    const QRect area = screen->availableGeometry();
    const int w = m_window->width();
    const int h = m_window->height();

    int x = area.right() - w - kMargin + 1;
    bool bottom = true;
    if (m_anchor) {
        x = qBound(area.left() + kMargin, m_anchor->x() - w / 2, qMax(area.left() + kMargin, area.right() - w - kMargin + 1));
        bottom = m_anchor->y() >= screen->geometry().center().y();
    }

    if (m_layerShell) {
        configureLayerShell(screen, x - screen->geometry().x(), bottom, m_anchor.has_value());
        return;
    }
    m_window->setScreen(screen);
    m_window->setPosition(x, bottom ? area.bottom() - h - kMargin + 1 : area.top() + kMargin);
}

void Popup::configureLayerShell(QScreen *screen, int left, bool bottom, bool anchored)
{
#ifdef HAVE_LAYERSHELLQT
    auto *lw = LayerShellQt::Window::get(m_window);
    if (!lw) {
        return;
    }
    if (m_window->screen() != screen) {
        m_window->setScreen(screen);
    }
    LayerShellQt::Window::Anchors anchors(bottom ? LayerShellQt::Window::AnchorBottom : LayerShellQt::Window::AnchorTop);
    anchors |= anchored ? LayerShellQt::Window::AnchorLeft : LayerShellQt::Window::AnchorRight;
#ifdef HAVE_LAYERSHELLQT_SCREEN
    // QWindow::screen() alone is not enough: the compositor picks the output
    lw->setScreen(screen);
#endif
    lw->setScope(QStringLiteral("onedriver-activity"));
    lw->setLayer(LayerShellQt::Window::LayerTop);
    lw->setAnchors(anchors);
    // margins are measured from the screen edge, or from the panel if there is one
    lw->setMargins(anchored ? QMargins(left, kMargin, 0, kMargin) : QMargins(0, 0, kMargin, kMargin));
    lw->setExclusiveZone(0);
    lw->setKeyboardInteractivity(LayerShellQt::Window::KeyboardInteractivityOnDemand);
#else
    Q_UNUSED(screen) Q_UNUSED(left) Q_UNUSED(bottom) Q_UNUSED(anchored)
#endif
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
