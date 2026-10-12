// SPDX-License-Identifier: GPL-3.0-or-later
#include "../src/logic.h"

#include <QTemporaryDir>
#include <QTest>

#include <sys/xattr.h>

using namespace OneDriverDolphin;

class TstLogic : public QObject
{
    Q_OBJECT
private Q_SLOTS:
    void overlays_data()
    {
        QTest::addColumn<QString>("state");
        QTest::addColumn<bool>("pinned");
        QTest::addColumn<bool>("isDir");
        QTest::addColumn<QStringList>("expected");

        QTest::newRow("cloud file") << "cloud" << false << false << QStringList{"onedriver-emblem-cloud"};
        QTest::newRow("cloud dir") << "cloud" << false << true << QStringList{};
        QTest::newRow("pinned cloud file (downloading)") << "cloud" << true << false << QStringList{"onedriver-emblem-cloud"};
        QTest::newRow("local") << "local" << false << false << QStringList{"onedriver-emblem-local"};
        QTest::newRow("local pinned") << "local" << true << false << QStringList{"onedriver-emblem-pinned"};
        QTest::newRow("pinned dir") << "local" << true << true << QStringList{"onedriver-emblem-pinned"};
        QTest::newRow("modified") << "modified" << false << false << QStringList{"emblem-synchronizing-symbolic"};
        QTest::newRow("uploading pinned") << "uploading" << true << false << QStringList{"emblem-synchronizing-symbolic"};
        QTest::newRow("error") << "error" << true << false << QStringList{"emblem-error"};
        QTest::newRow("unknown") << "" << false << false << QStringList{};
    }
    void overlays()
    {
        QFETCH(QString, state);
        QFETCH(bool, pinned);
        QFETCH(bool, isDir);
        QFETCH(QStringList, expected);
        FileState st;
        st.state = state;
        st.pinned = pinned;
        QCOMPARE(overlaysFor(st, isDir), expected);
    }

    void mounts()
    {
        const QStringList m{"/home/u/OneDrive", "/home/u/OneDrive/nested", "/mnt/od"};
        QCOMPARE(mountForPath("/home/u/OneDrive/a/b.txt", m), QString("/home/u/OneDrive"));
        QCOMPARE(mountForPath("/home/u/OneDrive", m), QString("/home/u/OneDrive"));
        QCOMPARE(mountForPath("/home/u/OneDrive/nested/x", m), QString("/home/u/OneDrive/nested"));
        QCOMPARE(mountForPath("/home/u/OneDrive2/x", m), QString());
        QCOMPARE(mountForPath("/home/u/OneDriv", m), QString());
        QCOMPARE(mountForPath("/mnt/od/f", m), QString("/mnt/od"));
        QCOMPARE(mountForPath("/etc/passwd", m), QString());
        QCOMPARE(mountForPath("/etc/passwd", {}), QString());
    }

    void relAbs()
    {
        QCOMPARE(absolutePath("/m", ""), QString("/m"));
        QCOMPARE(absolutePath("/m", "."), QString("/m"));
        QCOMPARE(absolutePath("/m", "a/b"), QString("/m/a/b"));
        QCOMPARE(relativePath("/m", "/m"), QString());
        QCOMPARE(relativePath("/m", "/m/a/b"), QString("a/b"));
    }

    void busNames()
    {
        QVERIFY(isMountBusName("io.github.onedriver.Mount.home_leif_OneDrive"));
        QVERIFY(!isMountBusName("io.github.onedriver.Mount."));
        QVERIFY(!isMountBusName("io.github.onedriver.Activity"));
    }

    void xattrRoundTrip()
    {
        QTemporaryDir dir(QDir::homePath() + "/.onedriver-dolphin-test-XXXXXX");
        QVERIFY(dir.isValid());
        const QString f = dir.path() + "/f";
        QFile file(f);
        QVERIFY(file.open(QIODevice::WriteOnly));
        file.close();
        if (setxattr(f.toLocal8Bit().constData(), "user.onedriver.state", "cloud", 5, 0) != 0)
            QSKIP("filesystem has no user xattrs");
        QVERIFY(!readFileState(f).pinned);
        QCOMPARE(readFileState(f).state, QString("cloud"));
        setxattr(f.toLocal8Bit().constData(), "user.onedriver.pinned", "1", 1, 0);
        QVERIFY(readFileState(f).pinned);
        QVERIFY(!readFileState(dir.path() + "/missing").valid());
    }
};

QTEST_APPLESS_MAIN(TstLogic)
#include "tst_logic.moc"
