// SPDX-License-Identifier: GPL-3.0-or-later
// Smoke test: load the real overlay plugin and talk to a fake mount service.
// usage (inside dbus-run-session): smoke <plugin.so> <mountdir>
// The mount dir needs user xattrs; files a (cloud), b (local+pinned), c (modified) are expected.
#include <KOverlayIconPlugin>
#include <KPluginFactory>
#include <KPluginMetaData>

#include <QCoreApplication>
#include <QEventLoop>
#include <QDBusConnection>
#include <QDBusMessage>
#include <QElapsedTimer>
#include <QTimer>
#include <QUrl>

#include <cstdio>

class FakeMount : public QObject
{
    Q_OBJECT
    Q_CLASSINFO("D-Bus Interface", "io.github.onedriver.Mount1")
    Q_PROPERTY(QString MountPoint READ mountPoint)
public:
    QString mp;
    QString mountPoint() const { return mp; }
Q_SIGNALS:
    void FileStatesChanged(const QStringList &paths);
};

static int fails = 0;
#define CHECK(cond, msg)                                                                 \
    do {                                                                                 \
        if (!(cond)) {                                                                   \
            std::printf("FAIL: %s\n", msg);                                              \
            ++fails;                                                                     \
        } else                                                                           \
            std::printf("ok:   %s\n", msg);                                              \
    } while (0)

int main(int argc, char **argv)
{
    QCoreApplication app(argc, argv);
    if (argc < 3)
        return 2;
    const QString dir = QString::fromLocal8Bit(argv[2]);

    QDBusConnection fake = QDBusConnection::connectToBus(QDBusConnection::SessionBus, QStringLiteral("fake"));
    FakeMount mount;
    mount.mp = dir;
    fake.registerService(QStringLiteral("io.github.onedriver.Mount.fake"));
    fake.registerObject(QStringLiteral("/io/github/onedriver/Mount"), &mount, QDBusConnection::ExportAllProperties | QDBusConnection::ExportAllSignals);

    const KPluginMetaData md(QString::fromLocal8Bit(argv[1]));
    CHECK(md.isValid(), "plugin metadata valid");
    auto res = KPluginFactory::instantiatePlugin<KOverlayIconPlugin>(md);
    CHECK(bool(res.plugin), "plugin instantiates");
    if (!res.plugin)
        return 1;
    KOverlayIconPlugin *p = res.plugin;

    QUrl outside = QUrl::fromLocalFile(QStringLiteral("/etc/passwd"));
    QElapsedTimer t;
    t.start();
    CHECK(p->getOverlays(outside).isEmpty(), "outside mount: empty");

    // let discovery finish
    QEventLoop loop;
    QTimer::singleShot(1000, &loop, &QEventLoop::quit);
    loop.exec();

    CHECK(p->getOverlays(QUrl::fromLocalFile(dir + "/a")) == QStringList{"onedriver-emblem-cloud"}, "cloud file -> cloud emblem");
    CHECK(p->getOverlays(QUrl::fromLocalFile(dir + "/b")) == QStringList{"emblem-success"}, "local+pinned -> emblem-success");
    CHECK(p->getOverlays(QUrl::fromLocalFile(dir + "/c")) == QStringList{"emblem-synchronizing-symbolic"}, "modified -> sync emblem");
    CHECK(p->getOverlays(QUrl::fromLocalFile(dir + "/none")).isEmpty(), "missing file -> empty");

    QUrl changedUrl;
    QStringList changedOv;
    QObject::connect(p, &KOverlayIconPlugin::overlaysChanged, [&](const QUrl &u, const QStringList &o) {
        changedUrl = u;
        changedOv = o;
    });
    Q_EMIT mount.FileStatesChanged({QStringLiteral("a")});
    QTimer::singleShot(1000, &loop, &QEventLoop::quit);
    loop.exec();
    CHECK(changedUrl == QUrl::fromLocalFile(dir + "/a"), "FileStatesChanged -> overlaysChanged url");
    CHECK(changedOv == QStringList{"onedriver-emblem-cloud"}, "FileStatesChanged -> overlays recomputed");
    std::printf("%d failures\n", fails);
    return fails ? 1 : 0;
}

#include "smoke.moc"
